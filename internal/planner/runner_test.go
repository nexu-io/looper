package planner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/specpr"
	"github.com/nexu-io/looper/internal/lifecycle"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/storage"
)

func TestValidatedSpecApprovalReceiptRequiresRemoteTimestamp(t *testing.T) {
	for _, comment := range []planedoc.PageComment{
		{ID: "request"},
		{ID: "request", CreatedAt: "not-a-time"},
		{CreatedAt: "2026-07-17T12:00:00Z"},
	} {
		if _, _, err := validatedSpecApprovalReceipt(comment); err == nil {
			t.Fatalf("invalid receipt unexpectedly accepted: %#v", comment)
		}
	}
	if id, createdAt, err := validatedSpecApprovalReceipt(planedoc.PageComment{ID: " request ", CreatedAt: "2026-07-17T12:00:00Z"}); err != nil || id != "request" || createdAt != "2026-07-17T12:00:00Z" {
		t.Fatalf("valid receipt = %q, %q, %v", id, createdAt, err)
	}
}

func TestReviewRejectsPlanePageEditedWhileIndependentReviewerRuns(t *testing.T) {
	ctx := context.Background()
	fixture := newRunnerFixture(t)
	worktreePath := t.TempDir()
	specPath := "specs/change.md"
	if err := os.MkdirAll(filepath.Join(worktreePath, "specs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, specPath), []byte("# 已复核方案\n原始内容\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	originalRendered := "<h1>已复核方案</h1><p>原始内容</p>"
	editedDuringReview := "<h1>未复核的新版本</h1><p>人工在 REVIEW 运行中改动</p>"
	gw, calls := scriptedGateway(
		`{"results":[{"id":"l1","title":"looper:tech-spec","url":"https://plane.x/pages/pg-1"}]}`,
		editedDuringReview,
	)
	cfg := config.Config{Projects: []config.ProjectRefConfig{{ID: "project_1", Owner: &config.FeishuActorConfig{PlaneID: "owner-plane-id"}}}}
	runner := New(Options{
		DB: fixture.coordinator.DB(), Repos: fixture.repos,
		Git:                &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "review-head"}},
		AgentExecutor:      &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "VERDICT: READY — 可以实现"}}},
		Logger:             fixture.logger,
		Now:                fixture.now,
		CustomInstructions: &cfg,
		PlaneDoc: func(string) (*planedoc.Gateway, string, bool) {
			return gw, "plane-project", true
		},
	})

	loopID, runID := "loop-review-race", "run-review-race"
	now := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "running", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	checkpoint := plannerCheckpoint{
		Issue:    &checkpointIssue{Repo: "nexu-io/open-design", IssueNumber: 582, Title: "导出", URL: "https://plane.x/projects/p/issues/wi-1", SpecPath: specPath},
		Worktree: &checkpointWorktree{Path: worktreePath, BaseBranch: "main", SpecPath: specPath},
		Publish:  &checkpointPublishState{Grilled: true, ReviewPlaneContentHash: contentSHA256(originalRendered)},
	}
	encoded := mustMarshalJSON(checkpoint)
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", CheckpointJSON: &encoded, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	input := stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Loop: storage.LoopRecord{ID: loopID, ProjectID: "project_1"}, Run: storage.RunRecord{ID: runID, LoopID: loopID}, Checkpoint: checkpoint}
	got, err := runner.runReviewStep(ctx, input)
	var le *loopError
	if !errors.As(err, &le) || le.kind != FailureRetryableAfterResume || !strings.Contains(err.Error(), "changed during independent REVIEW") {
		t.Fatalf("runReviewStep() error = %v, want fail-closed REVIEW race", err)
	}
	if got.Publish == nil || got.Publish.ReviewPlaneContentHash != "" || got.Publish.Reviewed {
		t.Fatalf("approval state after race = %#v, want hash invalidated and no approval", got.Publish)
	}
	if len(*calls) != 2 {
		t.Fatalf("Plane calls = %#v, want link lookup + post-REVIEW page read only (no approval comment)", *calls)
	}
}

func TestBuildPlannerPromptUsesConcreteDisclosureMetadata(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	prompt, _ := buildPlannerPrompt(storage.ProjectRecord{ID: "project_1", RepoPath: repoPath}, customInstructionConfig(nil), &checkpointIssue{Repo: "acme/looper", IssueNumber: 156, Title: "fix disclosure", SpecPath: "docs/spec.md"}, &checkpointWorktree{Branch: "looper/fix", BaseBranch: "main"}, true, config.DefaultDisclosureConfig(), "opencode", "openai/gpt-5.5")
	for _, want := range []string{"agent=opencode"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	for _, unwanted := range []string{"agent=<agent-runtime>", "model=<agent-model>", "model=openai/gpt-5.5", "agent=gpt-5.5", "agent=openai/gpt-5.5"} {
		if strings.Contains(prompt, unwanted) {
			t.Fatalf("prompt contains %q:\n%s", unwanted, prompt)
		}
	}
}

func TestCommitGrillSpecRevisionCommitsOnlyTheSpecFile(t *testing.T) {
	project := storage.ProjectRecord{ID: "project", RepoPath: t.TempDir()}
	worktree := checkpointWorktree{Path: filepath.Join(t.TempDir(), "worktree"), Branch: "looper/spec", BaseBranch: "main", SpecPath: "specs/change.md"}
	git := &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "base-sha", HasUncommittedChanges: true, ChangedFiles: []string{"specs/change.md"}}, commitResult: CommitResult{CommitSHA: "grill-sha"}}
	runner := &Runner{git: git}
	checkpoint, err := runner.commitGrillSpecRevision(context.Background(), stepInput{Project: project}, plannerCheckpoint{}, checkpointIssue{Title: "导出方案"}, worktree, worktree.SpecPath, "base-sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(git.commitCalls) != 1 || checkpoint.Lifecycle == nil || len(checkpoint.Lifecycle.CommitSHAs) != 1 || checkpoint.Lifecycle.CommitSHAs[0] != "grill-sha" {
		t.Fatalf("commit/lifecycle = %#v, calls=%#v", checkpoint.Lifecycle, git.commitCalls)
	}

	unsafeGit := &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "base-sha", HasUncommittedChanges: true, ChangedFiles: []string{"apps/web.ts"}}}
	_, err = (&Runner{git: unsafeGit}).commitGrillSpecRevision(context.Background(), stepInput{Project: project}, plannerCheckpoint{}, checkpointIssue{Title: "x"}, worktree, worktree.SpecPath, "base-sha")
	if err == nil || len(unsafeGit.commitCalls) != 0 {
		t.Fatalf("non-spec edit must fail before commit: err=%v calls=%#v", err, unsafeGit.commitCalls)
	}
}

func TestCommitGrillSpecRevisionRecoversDurableProductAskAfterCommitCheckpointFailure(t *testing.T) {
	project := storage.ProjectRecord{ID: "project", RepoPath: t.TempDir()}
	worktreePath := t.TempDir()
	specPath := "specs/change.md"
	if err := os.MkdirAll(filepath.Join(worktreePath, "specs"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "# revised spec\n"
	if err := os.WriteFile(filepath.Join(worktreePath, specPath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	checkpoint := plannerCheckpoint{Publish: &checkpointPublishState{
		GrillAgentCompleted: true,
		GrillBaselineHead:   "base-sha",
		GrillProductAsk:     "RETURN_TO_REQUIREMENTS: 重新确认产品边界",
		GrillSpecHash:       fmt.Sprintf("%x", sum[:]),
	}}
	git := &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "looper-grill-commit", CommittedChangedFiles: []string{specPath}, HasUncommittedChanges: false}}
	runner := &Runner{git: git}
	got, err := runner.commitGrillSpecRevision(context.Background(), stepInput{Project: project}, checkpoint, checkpointIssue{Title: "导出方案"}, checkpointWorktree{Path: worktreePath, Branch: "looper/spec", BaseBranch: "main", SpecPath: specPath}, specPath, "base-sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(git.commitCalls) != 0 || got.Publish == nil || got.Publish.GrillProductAsk == "" || got.Lifecycle == nil || len(got.Lifecycle.CommitSHAs) != 1 || got.Lifecycle.CommitSHAs[0] != "looper-grill-commit" {
		t.Fatalf("recovered checkpoint=%#v lifecycle=%#v commitCalls=%#v", got.Publish, got.Lifecycle, git.commitCalls)
	}
}

func TestVerifyDurableGrillSpecSupportsPostPublishRetryWithoutAnotherCommit(t *testing.T) {
	project := storage.ProjectRecord{ID: "project", RepoPath: t.TempDir()}
	worktreePath := t.TempDir()
	specPath := "specs/change.md"
	if err := os.MkdirAll(filepath.Join(worktreePath, "specs"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "# durable grill spec\n"
	if err := os.WriteFile(filepath.Join(worktreePath, specPath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	git := &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "grill-commit", HasUncommittedChanges: false}}
	runner := &Runner{git: git}
	worktree := checkpointWorktree{Path: worktreePath, Branch: "looper/spec", BaseBranch: "main", SpecPath: specPath}
	if _, err := runner.verifyDurableGrillSpec(context.Background(), project, worktree, specPath, fmt.Sprintf("%x", sum[:]), true); err != nil {
		t.Fatal(err)
	}
	if len(git.commitCalls) != 0 {
		t.Fatalf("post-publish retry attempted another commit: %#v", git.commitCalls)
	}
	if err := os.WriteFile(filepath.Join(worktreePath, specPath), []byte("# changed after checkpoint\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.verifyDurableGrillSpec(context.Background(), project, worktree, specPath, fmt.Sprintf("%x", sum[:]), true); err == nil {
		t.Fatal("changed spec bytes must not pass a durable GRILL retry")
	}
}

func TestCommitGrillSpecRevisionRejectsRecoveryCommitWithBusinessFiles(t *testing.T) {
	project := storage.ProjectRecord{ID: "project", RepoPath: t.TempDir()}
	worktreePath := t.TempDir()
	specPath := "specs/change.md"
	if err := os.MkdirAll(filepath.Join(worktreePath, "specs"), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "# revised spec\n"
	if err := os.WriteFile(filepath.Join(worktreePath, specPath), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	checkpoint := plannerCheckpoint{Publish: &checkpointPublishState{GrillAgentCompleted: true, GrillBaselineHead: "base-sha", GrillSpecHash: fmt.Sprintf("%x", sum[:])}}
	git := &fakeGitGateway{inspectResult: InspectHeadResult{HeadSHA: "agent-commit", CommittedChangedFiles: []string{specPath, "apps/web.ts"}}}
	_, err := (&Runner{git: git}).commitGrillSpecRevision(context.Background(), stepInput{Project: project}, checkpoint, checkpointIssue{Title: "x"}, checkpointWorktree{Path: worktreePath, BaseBranch: "main"}, specPath, "base-sha")
	if err == nil || !strings.Contains(err.Error(), "agent created commits") {
		t.Fatalf("business-file agent commit was accepted: %v", err)
	}
}

func TestPostNodeHThreadNoteUsesStableUUIDWithoutLegacyTransport(t *testing.T) {
	var uuids []string
	runner := &Runner{postThreadNoteWithUUID: func(_ context.Context, loopID, text string, mentions []string, uuid string) error {
		if loopID != "loop" || text != "approve" || len(mentions) != 0 {
			t.Fatalf("unexpected note: loop=%q text=%q mentions=%#v", loopID, text, mentions)
		}
		uuids = append(uuids, uuid)
		return nil
	}}
	input := stepInput{Loop: storage.LoopRecord{ID: "loop"}, Project: storage.ProjectRecord{ID: "project"}}
	runner.postNodeHThreadNote(context.Background(), input, "approval:hash", "approve", false)
	runner.postNodeHThreadNote(context.Background(), input, "approval:hash", "approve", false)
	if len(uuids) != 2 || uuids[0] == "" || uuids[0] != uuids[1] {
		t.Fatalf("UUIDs = %#v; crash retry must use one stable visible-message UUID", uuids)
	}
}

func TestBuildPlannerPromptOmitsMissingAgentRuntime(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	prompt, _ := buildPlannerPrompt(storage.ProjectRecord{ID: "project_1", RepoPath: repoPath}, customInstructionConfig(nil), &checkpointIssue{Repo: "acme/looper", IssueNumber: 156, Title: "fix disclosure", SpecPath: "docs/spec.md"}, &checkpointWorktree{Branch: "looper/fix", BaseBranch: "main"}, true, config.DefaultDisclosureConfig(), "", "openai/gpt-5.5")
	if strings.Contains(prompt, "agent=") {
		t.Fatalf("prompt should omit missing agent runtime:\n%s", prompt)
	}
	if strings.Contains(prompt, "model=") || strings.Contains(prompt, "openai/gpt-5.5") {
		t.Fatalf("prompt should not expose configured model:\n%s", prompt)
	}
}

func TestBuildPlannerPromptUsesForgejoIssueTextForForgejoProjects(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	cfg, err := config.Normalize("")
	if err != nil {
		t.Fatalf("Normalize() error = %v", err)
	}
	cfg.Providers = []config.ProviderConfig{{ID: "forgejo-main", Kind: config.ProviderKindForgejo, BaseURL: "https://forgejo.example.test", TokenEnv: stringPtr("FORGEJO_TOKEN")}}
	cfg.Projects = []config.ProjectRefConfig{{ID: "project_1", Provider: "forgejo-main", Repo: "acme/looper", RepoPath: repoPath}}
	prompt, _ := buildPlannerPrompt(storage.ProjectRecord{ID: "project_1", RepoPath: repoPath}, cfg, &checkpointIssue{Repo: "acme/looper", IssueNumber: 156, Title: "forgejo issue", SpecPath: "docs/spec.md"}, &checkpointWorktree{Branch: "looper/fix", BaseBranch: "main"}, true, config.DefaultDisclosureConfig(), "opencode", "model")
	if !strings.Contains(prompt, "Write a planning spec for Forgejo issue acme/looper#156.") {
		t.Fatalf("prompt = %q, want Forgejo issue text", prompt)
	}
	if strings.Contains(prompt, "GitHub issue") {
		t.Fatalf("prompt = %q, want no GitHub-specific issue text", prompt)
	}
}

func TestBuildPlannerPromptMakesProductSpecAuthoritative(t *testing.T) {
	t.Parallel()

	project := storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}
	issue := &checkpointIssue{
		Repo:           "nexu-io/open-design",
		IssueNumber:    582,
		Title:          "高保真导出",
		Body:           "建议先做 HTML",
		SpecPath:       "specs/582.md",
		ProductSpecURL: "https://plane.test/pages/product-582",
		ProductSpec:    "目标：第一阶段提供 React + 独立 CSS；保留现有 HTML 入口。",
	}
	prompt, _ := buildPlannerPrompt(project, customInstructionConfig(nil), issue, &checkpointWorktree{Branch: "looper/582", BaseBranch: "main"}, false, config.DefaultDisclosureConfig(), "", "")

	for _, want := range []string{"AUTHORITATIVE PRODUCT SPEC", "第一阶段提供 React + 独立 CSS", "highest-priority source of truth", "Do not replace an explicit product decision", "entire technical spec in Simplified Chinese"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestPlannerReviewPromptsPreserveChineseTechnicalSpec(t *testing.T) {
	t.Parallel()
	issue := checkpointIssue{Repo: "nexu-io/open-design", IssueNumber: 582}
	if prompt := buildGrillPrompt(issue, "specs/582.md", ""); !strings.Contains(prompt, "entire technical spec in Simplified Chinese") {
		t.Fatalf("grill prompt missing Chinese-spec requirement:\n%s", prompt)
	}
	if prompt := buildReviewPrompt(issue, "specs/582.md", ""); !strings.Contains(prompt, "technical spec itself is written in Simplified Chinese") || !strings.Contains(prompt, "VERDICT: READY") {
		t.Fatalf("review prompt missing Chinese spec/verdict requirement:\n%s", prompt)
	}
}

func TestShouldRetryQueueFailureRespectsMaxAttempts(t *testing.T) {
	t.Parallel()

	if !shouldRetryQueueFailure(FailureRetryableTransient, 5, -1) {
		t.Fatal("shouldRetryQueueFailure() = false, want true for infinite retries")
	}
	if shouldRetryQueueFailure(FailureNonRetryable, 5, -1) {
		t.Fatal("shouldRetryQueueFailure() = true, want false for infinite non_retryable retries")
	}
	if !shouldRetryQueueFailure(FailureNonRetryable, 1, 3) {
		t.Fatal("shouldRetryQueueFailure() = false, want true for bounded non_retryable retries")
	}
	if shouldRetryQueueFailure(FailureRetryableTransient, 3, 3) {
		t.Fatal("shouldRetryQueueFailure() = true, want false once nextAttempts reaches maxAttempts")
	}
}

func TestBackoffDelayCapsInfiniteRetryOverflow(t *testing.T) {
	t.Parallel()

	if got := backoffDelay(defaultRetryDelay, 100); got != maxRetryDelay {
		t.Fatalf("backoffDelay(infinite retry overflow) = %v, want %v", got, maxRetryDelay)
	}
}

func TestNewPreservesInfiniteRetryMaxAttempts(t *testing.T) {
	t.Parallel()

	runner := New(Options{RetryMaxAttempts: -1})
	if runner.retryMaxAttempts != -1 {
		t.Fatalf("retryMaxAttempts = %d, want -1", runner.retryMaxAttempts)
	}
}

func TestDiscoverIssuesEnqueuesEligibleWorkAndCreatesLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, {Number: 43, Title: "Skip", Assignees: []string{"someone"}, Labels: []string{"looper:plan"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want one queue item and one loop", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Type != "planner" || queue.DedupeKey != "planner:project_1:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue = %#v, want planner queue for issue 42", queue)
	}
}

func TestDiscoverIssuesSkipsGlobalHoldLabel(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan", domain.HoldLabelGlobal}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 || len(result.CreatedLoopIDs) != 0 || result.Skipped != 1 {
		t.Fatalf("result = %#v, want held issue skipped", result)
	}
}

func TestProcessClaimedItemSkipsHeldPlannerIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repo := "acme/looper"
	issueNumber := int64(42)
	loopTarget := buildIssueTargetID(repo, issueNumber)
	nowISO := fixture.nowISO()
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_hold", Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &loopTarget, Repo: &repo, Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_planner_hold"
	lockKey := storage.IssueLockKey(projectID, repo, issueNumber)
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_hold", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: &repo, DedupeKey: "planner:hold", Priority: storage.QueuePriorityPlanner, Status: "running", AvailableAt: nowISO, LockKey: &lockKey, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: issueNumber, Labels: []string{domain.HoldLabelGlobal}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_planner_hold", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: &repo, Status: "running"})
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	loop, _ := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued", loop)
	}
	queue, _ := fixture.repos.Queue.GetByID(context.Background(), "queue_planner_hold")
	if queue == nil || queue.Status != "completed" {
		t.Fatalf("queue = %#v, want completed", queue)
	}
	if len(github.createPRCalls) != 0 {
		t.Fatalf("createPRCalls = %#v, want none", github.createPRCalls)
	}
}

func TestRunPublishStepSkipsWhenHoldAddedBeforePush(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Labels: []string{domain.HoldLabelGlobal}}}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	checkpoint, err := runner.runPublishStep(context.Background(), stepInput{Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()}, Checkpoint: plannerCheckpoint{Issue: &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this"}, Worktree: &checkpointWorktree{Path: t.TempDir(), Branch: "planner/42", BaseBranch: "main"}, WriteSpec: &checkpointWriteSpec{Status: "completed"}}})
	if err == nil || !strings.Contains(err.Error(), "currently held") {
		t.Fatalf("runPublishStep() error = %v, want hold skip", err)
	}
	if checkpoint.SkipReason != "" {
		t.Fatalf("checkpoint = %#v, want unchanged checkpoint before hold handling", checkpoint)
	}
	if len(git.pushCalls) != 0 {
		t.Fatalf("pushCalls = %#v, want none", git.pushCalls)
	}
}

func TestRunPublishStepRechecksHoldAfterCreatingPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		issueDetails: []IssueDetail{
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan"}},
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan"}},
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan", domain.HoldLabelGlobal}},
		},
		createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"},
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	loopID := "loop_planner_publish_hold_after_pr"
	runID := "run_planner_publish_hold_after_pr"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runPublishStep(context.Background(), stepInput{
		Project:   storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:      storage.LoopRecord{ID: loopID, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running"},
		Run:       storage.RunRecord{ID: runID, LoopID: loopID},
		QueueItem: storage.QueueItemRecord{ID: "queue_1", PayloadJSON: stringPtr(`{"issueNumber":42}`)},
		Checkpoint: plannerCheckpoint{
			Issue:     &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", RequestedReviewers: []string{"teammate"}},
			Worktree:  &checkpointWorktree{Path: t.TempDir(), Branch: "planner/42", BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "currently held") {
		t.Fatalf("runPublishStep() error = %v, want hold skip after PR create", err)
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("pushCalls = %#v, want one push before post-PR hold", git.pushCalls)
	}
	if len(github.createPRCalls) != 1 {
		t.Fatalf("createPRCalls = %#v, want one created PR before post-PR hold", github.createPRCalls)
	}
	if checkpoint.Publish == nil || checkpoint.Publish.PullRequest == nil || checkpoint.Publish.PullRequest.Number != 101 {
		t.Fatalf("checkpoint.Publish = %#v, want PR reference preserved before hold skip", checkpoint.Publish)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-reviewing label after post-PR hold", github.addLabelCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no reviewers after post-PR hold", github.addReviewerCalls)
	}
}

func TestRunPublishStepRechecksPullRequestHoldAfterCreatingPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{
		issueDetails: []IssueDetail{
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan"}},
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan"}},
			{Number: 42, Title: "Plan this", Labels: []string{"looper:plan"}},
		},
		prDetail:       PullRequestDetail{Number: 101, Labels: []string{domain.HoldLabelGlobal}},
		createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"},
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	loopID := "loop_planner_publish_pr_hold_after_pr"
	runID := "run_planner_publish_pr_hold_after_pr"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runPublishStep(context.Background(), stepInput{
		Project:   storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir()},
		Loop:      storage.LoopRecord{ID: loopID, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running"},
		Run:       storage.RunRecord{ID: runID, LoopID: loopID},
		QueueItem: storage.QueueItemRecord{ID: "queue_1", PayloadJSON: stringPtr(`{"issueNumber":42}`)},
		Checkpoint: plannerCheckpoint{
			Issue:     &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", RequestedReviewers: []string{"teammate"}},
			Worktree:  &checkpointWorktree{Path: t.TempDir(), Branch: "planner/42", BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "acme/looper#101 is currently held") {
		t.Fatalf("runPublishStep() error = %v, want PR hold skip", err)
	}
	if checkpoint.Publish == nil || checkpoint.Publish.PullRequest == nil || checkpoint.Publish.PullRequest.Number != 101 {
		t.Fatalf("checkpoint.Publish = %#v, want PR reference preserved before PR hold skip", checkpoint.Publish)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no spec-reviewing label after PR hold", github.addLabelCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no reviewers after PR hold", github.addReviewerCalls)
	}
}

func TestRunPublishStepChecksLifecycleAdoptedPullRequestHoldBeforeDisclosure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loopID := "loop_planner_lifecycle_adopted_pr_hold"
	runID := "run_planner_lifecycle_adopted_pr_hold"
	repoPath := t.TempDir()
	branch := "planner/42"
	github := &fakeGitHubGateway{
		issueDetail: IssueDetail{Number: 42, Labels: []string{"looper:plan"}},
		prDetail:    PullRequestDetail{Number: 202, URL: "https://example/pr/202", State: "OPEN", HeadRefName: branch, BaseRefName: "main", Labels: []string{domain.HoldLabelGlobal}, Body: "## Summary\n\nExisting spec PR"},
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runPublishStep(context.Background(), stepInput{
		Project:   storage.ProjectRecord{ID: "project_1", RepoPath: repoPath},
		Loop:      storage.LoopRecord{ID: loopID, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running"},
		Run:       storage.RunRecord{ID: runID, LoopID: loopID},
		QueueItem: storage.QueueItemRecord{ID: "queue_1", PayloadJSON: stringPtr(`{"issueNumber":42}`)},
		Checkpoint: plannerCheckpoint{
			Issue:     &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", RequestedReviewers: []string{"teammate"}},
			Worktree:  &checkpointWorktree{Path: t.TempDir(), Branch: branch, BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
			Lifecycle: &lifecycle.State{Branch: branch, BaseBranch: "main", PRNumber: 202, PRURL: "https://example/pr/202", Actions: lifecycle.Actions{PR: lifecycle.ActionSourceAgent}},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "acme/looper#202 is currently held") {
		t.Fatalf("runPublishStep() error = %v, want adopted PR hold skip", err)
	}
	if checkpoint.Publish == nil || checkpoint.Publish.PullRequest != nil {
		t.Fatalf("checkpoint.Publish = %#v, want no adopted PR persisted before hold skip", checkpoint.Publish)
	}
	if len(github.updatePRBodyCalls) != 0 {
		t.Fatalf("updatePRBodyCalls = %#v, want no disclosure rewrite for held adopted PR", github.updatePRBodyCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no labels for held adopted PR", github.addLabelCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no reviewers for held adopted PR", github.addReviewerCalls)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.PRNumber != nil {
		t.Fatalf("loop = %#v, want no PR reference persisted", loop)
	}
}

func TestRunPublishStepChecksBranchAdoptedPullRequestHoldBeforeDisclosure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	loopID := "loop_planner_branch_adopted_pr_hold"
	runID := "run_planner_branch_adopted_pr_hold"
	repoPath := t.TempDir()
	branch := "planner/42"
	github := &fakeGitHubGateway{
		issueDetail:      IssueDetail{Number: 42, Labels: []string{"looper:plan"}},
		openPullRequests: []PullRequestSummary{{Number: 203, URL: "https://example/pr/203", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}},
		prDetail:         PullRequestDetail{Number: 203, URL: "https://example/pr/203", State: "OPEN", HeadRefName: branch, BaseRefName: "main", Labels: []string{domain.HoldLabelGlobal}, Body: "## Summary\n\nExisting spec PR"},
	}
	git := &fakeGitGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopID, Status: "running", StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}

	checkpoint, err := runner.runPublishStep(context.Background(), stepInput{
		Project:   storage.ProjectRecord{ID: "project_1", RepoPath: repoPath},
		Loop:      storage.LoopRecord{ID: loopID, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: stringPtr("issue:acme/looper:42"), Repo: stringPtr("acme/looper"), Status: "running"},
		Run:       storage.RunRecord{ID: runID, LoopID: loopID},
		QueueItem: storage.QueueItemRecord{ID: "queue_1", PayloadJSON: stringPtr(`{"issueNumber":42}`)},
		Checkpoint: plannerCheckpoint{
			Issue:     &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", RequestedReviewers: []string{"teammate"}},
			Worktree:  &checkpointWorktree{Path: t.TempDir(), Branch: branch, BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
		},
	})

	if err == nil || !strings.Contains(err.Error(), "acme/looper#203 is currently held") {
		t.Fatalf("runPublishStep() error = %v, want adopted PR hold skip", err)
	}
	if checkpoint.Publish == nil || checkpoint.Publish.PullRequest != nil {
		t.Fatalf("checkpoint.Publish = %#v, want no adopted PR persisted before hold skip", checkpoint.Publish)
	}
	if len(github.updatePRBodyCalls) != 0 {
		t.Fatalf("updatePRBodyCalls = %#v, want no disclosure rewrite for held adopted PR", github.updatePRBodyCalls)
	}
	if len(github.addLabelCalls) != 0 {
		t.Fatalf("addLabelCalls = %#v, want no labels for held adopted PR", github.addLabelCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no reviewers for held adopted PR", github.addReviewerCalls)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.PRNumber != nil {
		t.Fatalf("loop = %#v, want no PR reference persisted", loop)
	}
}

func TestDiscoverIssuesEnqueuesAcrossProjectsForSameIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := fixture.repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_2", Name: "Looper Duplicate", RepoPath: filepath.Join(t.TempDir(), "repo-2"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert(project_2) error = %v", err)
	}
	issue := IssueSummary{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	project1Loop, err := fixture.repos.Loops.GetByID(context.Background(), "missing")
	if err != nil || project1Loop != nil {
		t.Fatalf("Loops.GetByID(missing) = (%#v, %v), want (nil, nil)", project1Loop, err)
	}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue(project_1) error = %v", err)
	}
	project1Queue := storage.QueueItemRecord{ID: "queue_existing", ProjectID: stringPtr("project_1"), LoopID: &loopResult.record.ID, Type: "planner", TargetType: "issue", TargetID: buildIssueTargetID("acme/looper", issue.Number), Repo: stringPtr("acme/looper"), DedupeKey: buildPlannerDedupeKey("project_1", loopResult.record.ID, "acme/looper", issue.Number), Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 3, LockKey: stringPtr(storage.IssueLockKey("project_1", "acme/looper", issue.Number)), CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Queue.Upsert(context.Background(), project1Queue); err != nil {
		t.Fatalf("Queue.Upsert(existing) error = %v", err)
	}

	github := &fakeGitHubGateway{issues: []IssueSummary{issue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_2", Repo: "acme/looper"})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 || len(result.CreatedLoopIDs) != 1 {
		t.Fatalf("result = %#v, want one queue item and one loop", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), result.QueueItems[0].ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil {
		t.Fatal("Queue.GetByID() = nil, want created queue")
	}
	if queue.DedupeKey != "planner:project_2:"+result.CreatedLoopIDs[0]+":acme/looper:42" {
		t.Fatalf("queue.DedupeKey = %q, want project-scoped dedupe key", queue.DedupeKey)
	}
	allQueueItems, err := fixture.repos.Queue.List(context.Background())
	if err != nil {
		t.Fatalf("Queue.List() error = %v", err)
	}
	if len(allQueueItems) != 2 {
		t.Fatalf("len(Queue.List()) = %d, want 2", len(allQueueItems))
	}
}

func TestDiscoverIssuesUsesSingleServerSideLabelFilterWhenConfiguredWithMultipleLabels(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Labels: []string{"team:alpha", "team:beta"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha", "team:beta"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(github.listOpenIssueCalls) != 1 {
		t.Fatalf("listOpenIssueCalls = %#v, want one call", github.listOpenIssueCalls)
	}
	if got := github.listOpenIssueCalls[0].Labels; len(got) != 2 || got[0] != "team:alpha" || got[1] != "team:beta" {
		t.Fatalf("ListOpenIssues labels = %#v, want both configured labels", got)
	}
}

func TestDiscoverIssuesQueriesEachServerSideLabelWhenConfiguredWithAnyLabelMode(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 43, Title: "Plan any", Labels: []string{"team:beta"}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha", "team:beta"}, LabelMode: config.LabelModeAny, RequireAssigneeCurrentUser: false}})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(github.listOpenIssueCalls) != 2 {
		t.Fatalf("listOpenIssueCalls = %#v, want two calls", github.listOpenIssueCalls)
	}
	if github.listOpenIssueCalls[0].Label != "team:alpha" || github.listOpenIssueCalls[1].Label != "team:beta" {
		t.Fatalf("ListOpenIssues labels = [%q, %q], want configured labels", github.listOpenIssueCalls[0].Label, github.listOpenIssueCalls[1].Label)
	}
}

func TestDiscoverIssuesSkipsFailedPlannerLoopWhenFingerprintUnchanged(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	issue := IssueSummary{Number: 77, Title: "Plan this", Body: "same body", URL: "https://github.com/acme/looper/issues/77", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	fingerprint := buildPlannerDiscoveryFingerprint(repo, fixture.now(), issue)
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildIssueTargetID(repo, issue.Number)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_failed_same_fp", Seq: 88, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issues: []IssueSummary{issue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 0 {
		t.Fatalf("QueueItems = %#v, want none for unchanged failed fingerprint", result.QueueItems)
	}
}

func TestDiscoverIssuesRequeuesFailedPlannerLoopWhenFingerprintChanges(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	repo := "acme/looper"
	oldIssue := IssueSummary{Number: 78, Title: "Plan this", Body: "old body", URL: "https://github.com/acme/looper/issues/78", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	newIssue := IssueSummary{Number: 78, Title: "Plan this", Body: "new body", URL: "https://github.com/acme/looper/issues/78", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	fingerprint := buildPlannerDiscoveryFingerprint(repo, fixture.now(), oldIssue)
	metadata := fmt.Sprintf(`{"autonomousRecovery":{"lastFailedDiscoveryFingerprint":%q}}`, fingerprint)
	targetID := buildIssueTargetID(repo, newIssue.Number)
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_failed_changed_fp", Seq: 89, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "failed", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issues: []IssueSummary{newIssue}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	result, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: repo})
	if err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	if len(result.QueueItems) != 1 {
		t.Fatalf("QueueItems = %#v, want one queue item after fingerprint change", result.QueueItems)
	}
}

func TestRunPrepareWorktreeStepRecreatesUnsafeCheckpointAtRepoPath(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"},
			Worktree: &checkpointWorktree{Path: repoPath, Branch: "stale", BaseBranch: "main"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if checkpoint.ResumePolicy != "advance_from_checkpoint" {
		t.Fatalf("ResumePolicy = %q, want advance_from_checkpoint", checkpoint.ResumePolicy)
	}
}

func TestRunPrepareWorktreeStepRecreatesCheckpointOutsideWorktreeRoot(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	legacyPath := filepath.Join(t.TempDir(), "legacy-wt")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runPrepareWorktreeStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Checkpoint: plannerCheckpoint{
			Issue:    &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"},
			Worktree: &checkpointWorktree{Path: legacyPath, Branch: "stale", BaseBranch: "main"},
		},
	})
	if err != nil {
		t.Fatalf("runPrepareWorktreeStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if checkpoint.Worktree.Path == legacyPath {
		t.Fatalf("checkpoint.Worktree.Path = %q, want recreated path outside legacy worktree", checkpoint.Worktree.Path)
	}
	if git.createCalls[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %q, want %q", git.createCalls[0].WorktreeRoot, worktreeRoot)
	}
}

func TestRunWriteSpecStepRecreatesCheckpointOutsideWorktreeRootAndRunsAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	legacyPath := filepath.Join(t.TempDir(), "legacy-wt")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	issue := &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, issue.Repo, IssueSummary{Number: issue.IssueNumber, Title: issue.Title}, buildPlannerDiscoveryFingerprint(issue.Repo, fixture.now(), IssueSummary{Number: issue.IssueNumber, Title: issue.Title}))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	runID := "run_write_spec_rebuild"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopResult.record.ID, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runWriteSpecStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    loopResult.record,
		Run:     storage.RunRecord{ID: runID, LoopID: loopResult.record.ID},
		Checkpoint: plannerCheckpoint{
			Issue:     issue,
			Worktree:  &checkpointWorktree{Path: legacyPath, Branch: "stale", BaseBranch: "main"},
			WriteSpec: &checkpointWriteSpec{Status: "completed"},
		},
	})
	if err != nil {
		t.Fatalf("runWriteSpecStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want 1", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path != git.createResult.WorktreePath {
		t.Fatalf("checkpoint.Worktree = %#v, want recreated worktree", checkpoint.Worktree)
	}
	if git.createCalls[0].WorktreeRoot != worktreeRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %q, want %q", git.createCalls[0].WorktreeRoot, worktreeRoot)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if agent.starts[0].WorkingDirectory != git.createResult.WorktreePath {
		t.Fatalf("agent WorkingDirectory = %q, want rebuilt worktree %q", agent.starts[0].WorkingDirectory, git.createResult.WorktreePath)
	}
	if checkpoint.WriteSpec == nil || checkpoint.WriteSpec.Status != "completed" || !checkpoint.WriteSpec.GitReconciled {
		t.Fatalf("checkpoint.WriteSpec = %#v, want completed and reconciled write-spec after worktree recovery", checkpoint.WriteSpec)
	}
}

func TestRunWriteSpecStepRecreatesCleanedCheckpointWithinWorktreeRoot(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := filepath.Join(t.TempDir(), "worktrees")
	cleanedPath := filepath.Join(worktreeRoot, "looper-planner-42-plan-this")
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	issue := &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, issue.Repo, IssueSummary{Number: issue.IssueNumber, Title: issue.Title}, buildPlannerDiscoveryFingerprint(issue.Repo, fixture.now(), IssueSummary{Number: issue.IssueNumber, Title: issue.Title}))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	runID := "run_write_spec_cleaned_worktree"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopResult.record.ID, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{WorktreePath: filepath.Join(worktreeRoot, "restored"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	checkpoint, err := runner.runWriteSpecStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    loopResult.record,
		Run:     storage.RunRecord{ID: runID, LoopID: loopResult.record.ID},
		Checkpoint: plannerCheckpoint{
			Issue:    issue,
			Worktree: &checkpointWorktree{Path: cleanedPath, Branch: "looper/planner/42-plan-this", BaseBranch: "main"},
		},
	})
	if err != nil {
		t.Fatalf("runWriteSpecStep() error = %v", err)
	}
	if len(git.createCalls) != 1 {
		t.Fatalf("len(git.createCalls) = %d, want cleaned checkpoint recreated", len(git.createCalls))
	}
	if checkpoint.Worktree == nil || checkpoint.Worktree.Path == cleanedPath {
		t.Fatalf("checkpoint.Worktree = %#v, want replacement for cleaned path", checkpoint.Worktree)
	}
	if len(agent.starts) != 1 || agent.starts[0].WorkingDirectory != checkpoint.Worktree.Path {
		t.Fatalf("agent starts = %#v, want replacement worktree cwd %q", agent.starts, checkpoint.Worktree.Path)
	}
}

func TestRunWriteSpecStepRechecksPlannerHoldBeforeStartingAgent(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	repoPath := t.TempDir()
	worktreeRoot := t.TempDir()
	worktreePath := filepath.Join(worktreeRoot, "wt")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	issue := &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, issue.Repo, IssueSummary{Number: issue.IssueNumber, Title: issue.Title}, buildPlannerDiscoveryFingerprint(issue.Repo, fixture.now(), IssueSummary{Number: issue.IssueNumber, Title: issue.Title}))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	runID := "run_write_spec_held"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopResult.record.ID, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: issue.IssueNumber, Labels: []string{domain.HoldLabelGlobal}}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now})

	_, err = runner.runWriteSpecStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: repoPath, MetadataJSON: &metadata},
		Loop:    loopResult.record,
		Run:     storage.RunRecord{ID: runID, LoopID: loopResult.record.ID},
		Checkpoint: plannerCheckpoint{
			Issue:    issue,
			Worktree: &checkpointWorktree{Path: worktreePath, Branch: "looper/planner/42-plan-this", BaseBranch: "main"},
		},
	})
	var holdErr *holdSkipError
	if !errors.As(err, &holdErr) {
		t.Fatalf("runWriteSpecStep() error = %v, want holdSkipError", err)
	}
	if !strings.Contains(holdErr.summary, "acme/looper#42") {
		t.Fatalf("hold summary = %q, want held issue reference", holdErr.summary)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want agent not started", len(agent.starts))
	}
}

func TestRunWriteSpecStepRechecksPlannerHoldAfterAgentCompletion(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	worktreeRoot := t.TempDir()
	worktreePath := filepath.Join(worktreeRoot, "wt")
	if err := os.MkdirAll(worktreePath, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	metadata := fmt.Sprintf(`{"worktreeRoot":%q}`, worktreeRoot)
	issue := &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, issue.Repo, IssueSummary{Number: issue.IssueNumber, Title: issue.Title}, buildPlannerDiscoveryFingerprint(issue.Repo, fixture.now(), IssueSummary{Number: issue.IssueNumber, Title: issue.Title}))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	runID := "run_write_spec_held_after_agent"
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: runID, LoopID: loopResult.record.ID, Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetails: []IssueDetail{{Number: 42}, {Number: 42, Labels: []string{domain.HoldLabelGlobal}}}}
	git := &fakeGitGateway{inspectResult: InspectHeadResult{HasUncommittedChanges: true}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now})

	_, err = runner.runWriteSpecStep(context.Background(), stepInput{
		Project: storage.ProjectRecord{ID: "project_1", RepoPath: t.TempDir(), MetadataJSON: &metadata},
		Loop:    loopResult.record, Run: storage.RunRecord{ID: runID, LoopID: loopResult.record.ID},
		Checkpoint: plannerCheckpoint{Issue: issue, Worktree: &checkpointWorktree{Path: worktreePath, Branch: "looper/planner/42-plan-this", BaseBranch: "main"}},
	})
	var holdErr *holdSkipError
	if !errors.As(err, &holdErr) {
		t.Fatalf("runWriteSpecStep() error = %v, want holdSkipError", err)
	}
	if len(git.inspectCalls) != 0 || len(git.commitCalls) != 0 {
		t.Fatalf("git reconciliation calls = inspect %d, commit %d; want none", len(git.inspectCalls), len(git.commitCalls))
	}
}

func TestProcessClaimedItemManualPlannerBypassesDiscoveryChecks(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})
	github.createPRResult = CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	// Recovery can complete the queue record while the original worker is still
	// finishing. That benign race must not leave the successfully finished loop
	// stuck in running forever.
	if err := fixture.repos.Queue.Complete(context.Background(), claimed.ID, fixture.nowISO()); err != nil {
		t.Fatalf("Queue.Complete() precondition error = %v", err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("result = %#v, want success", result)
	}
	if result.PullRequestNumber != 101 {
		t.Fatalf("result.PullRequestNumber = %d, want 101", result.PullRequestNumber)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts = %d, want 1", len(agent.starts))
	}
	completed, err := fixture.repos.Loops.GetByID(context.Background(), loopResult.record.ID)
	if err != nil || completed == nil || completed.Status != "completed" {
		t.Fatalf("loop after success = %#v, %v; want completed", completed, err)
	}
}

func TestProcessClaimedItemPlannerSkipsWhenNotManualAndIneligible(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	if result.FailureKind != "" {
		t.Fatalf("result.FailureKind = %q, want empty", result.FailureKind)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want 0", len(agent.starts))
	}
}

func TestProcessClaimedItemDiscoveryQueueIgnoresManualLoopMetadata(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	loop := loopResult.record
	loop.MetadataJSON = stringPtr(`{"manual":true,"issueNumber":42}`)
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loop.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"someone"}, Labels: []string{"not-looper:plan"}, Body: "details", URL: "https://example/issues/42"}}
	agent := &fakeAgentExecutor{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claimed, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claimed == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claimed, err)
	}
	if claimed.ID != queueItem.ID {
		t.Fatalf("claimed.ID = %q, want %q", claimed.ID, queueItem.ID)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claimed)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "skipped" {
		t.Fatalf("result = %#v, want skipped", result)
	}
	if len(agent.starts) != 0 {
		t.Fatalf("len(agent.starts) = %d, want 0", len(agent.starts))
	}
}

func TestProcessClaimedItemSuccessfulPlannerPublish(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Stdout: "done"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want success with PR 101", result)
	}
	if len(agent.starts) != 1 || len(git.pushCalls) != 1 || len(github.createPRCalls) != 1 {
		t.Fatalf("agent starts=%d push=%d createPR=%d, want 1/1/1", len(agent.starts), len(git.pushCalls), len(github.createPRCalls))
	}
	if len(github.addLabelCalls) != 1 || len(github.addLabelCalls[0].Labels) != 1 || github.addLabelCalls[0].Labels[0] != specpr.ReviewingLabel {
		t.Fatalf("addLabelCalls = %#v, want spec-reviewing label", github.addLabelCalls)
	}
	if got := github.createPRCalls[0].Body; !strings.Contains(got, "\nSpec: ") {
		t.Fatalf("createPR body = %q, want Spec path line", got)
	}
	if !strings.Contains(agent.starts[0].Prompt, "When finished, print exactly one final line to stdout in this format:") {
		t.Fatalf("prompt = %q, want completion instruction", agent.starts[0].Prompt)
	}
	if !strings.Contains(agent.starts[0].Prompt, `__LOOPER_RESULT__={"summary":"<one-sentence summary>"}`) {
		t.Fatalf("prompt = %q, want canonical completion marker", agent.starts[0].Prompt)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "completed" || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prUrl":"https://example/pr/101"`) {
		t.Fatalf("loop = %#v, want completed loop with prUrl metadata", loop)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.CheckpointJSON == nil || !strings.Contains(*run.CheckpointJSON, `"id":"worktree_1"`) {
		t.Fatalf("run = %#v, want checkpoint with worktree id", run)
	}
}

func TestPlannerAdoptionHoldSummaryRechecksIssueBeforeAdoptedPullRequest(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Labels: []string{domain.HoldLabelGlobal}}}
	runner := New(Options{GitHub: github, Logger: fixture.logger, Now: fixture.now})
	checkpoint := plannerCheckpoint{Issue: &checkpointIssue{Repo: "acme/looper", IssueNumber: 42}}

	held, summary, err := runner.plannerAdoptionHoldSummary(context.Background(), storage.ProjectRecord{RepoPath: t.TempDir()}, checkpoint, "acme/looper", 101, storage.QueueItemRecord{})
	if err != nil {
		t.Fatalf("plannerAdoptionHoldSummary() error = %v", err)
	}
	if !held || !strings.Contains(summary, "acme/looper#42") {
		t.Fatalf("held, summary = %v, %q, want newly held source issue", held, summary)
	}
	if len(github.listOpenPRCalls) != 0 {
		t.Fatalf("listOpenPRCalls = %#v, want no further PR work after issue hold", github.listOpenPRCalls)
	}
}

func TestProcessClaimedItemUsesConfiguredPlannerPolicyLabels(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Labels: []string{"team:alpha"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Labels: []string{"team:alpha"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, loginErr: fmt.Errorf("login unavailable")}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true), DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 101 {
		t.Fatalf("result = %#v, want success with PR 101", result)
	}
	if len(github.addAssigneeCalls) != 0 {
		t.Fatalf("addAssigneeCalls = %#v, want no self-assignment when assignee policy is disabled", github.addAssigneeCalls)
	}
	if github.loginCalls != 0 {
		t.Fatalf("loginCalls = %d, want no login lookup when assignee policy is disabled", github.loginCalls)
	}
}

func TestProcessClaimedItemExcludesCurrentUserFromAssigneeReviewersWhenAssigneePolicyDisabled(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"team:alpha"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"team:alpha"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true), DiscoveryPolicy: DiscoveryPolicy{AutoDiscovery: true, Labels: []string{"team:alpha"}, LabelMode: config.LabelModeAll, RequireAssigneeCurrentUser: false}})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if github.loginCalls != 1 {
		t.Fatalf("loginCalls = %d, want login lookup before resolving assignee reviewers", github.loginCalls)
	}
	if len(github.addReviewerCalls) != 0 {
		t.Fatalf("addReviewerCalls = %#v, want no self review request", github.addReviewerCalls)
	}
}

func TestProcessClaimedItemSelfAssignsIssue(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"teammate"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if claim.ID != queueItem.ID {
		t.Fatalf("claim.ID = %q, want %q", claim.ID, queueItem.ID)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if len(github.addAssigneeCalls) != 1 {
		t.Fatalf("addAssigneeCalls = %#v, want one self-assignment", github.addAssigneeCalls)
	}
	call := github.addAssigneeCalls[0]
	if call.Repo != "acme/looper" || call.IssueNumber != 42 || len(call.Assignees) != 1 || call.Assignees[0] != "octocat" {
		t.Fatalf("add assignee call = %#v, want octocat on acme/looper#42", call)
	}
}

func TestProcessClaimedItemSurfacesIssueSelfAssignmentFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this"}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	if _, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number, "manual": true}}); err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	github := &fakeGitHubGateway{issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"teammate"}, Labels: []string{"looper:plan"}}, addAssigneeErr: fmt.Errorf("permission denied")}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedQueueItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedQueueItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureRetryableAfterResume || !strings.Contains(result.Summary, "Unable to assign issue acme/looper#42 to octocat") {
		t.Fatalf("result = %#v, want clear retryable assignment failure", result)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: "issue:acme/looper:42", Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want assignment failure to release issue lock")
	}
}

func TestProcessClaimedItemAdoptsOpenBranchPRWithoutRewritingHumanBody(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := "looper/planner/42-plan-this"
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, openPullRequests: []PullRequestSummary{{Number: 202, URL: "https://example/pr/202", State: "OPEN", HeadRefName: branch, BaseRefName: "main"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Lifecycle: &lifecycle.State{Branch: branch, BaseBranch: "main", PRURL: "https://example/pr/202", Actions: lifecycle.Actions{PR: lifecycle.ActionSourceAgent}}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 202 {
		t.Fatalf("result = %#v, want success with adopted PR 202", result)
	}
	if len(github.createPRCalls) != 0 {
		t.Fatalf("createPRCalls = %#v, want no fallback CreatePullRequest", github.createPRCalls)
	}
	if len(github.updatePRBodyCalls) != 0 {
		t.Fatalf("updatePRBodyCalls = %#v, want no body rewrite for human-authored PR", github.updatePRBodyCalls)
	}
	if len(github.listOpenPRCalls) != 1 {
		t.Fatalf("listOpenPRCalls = %d, want 1", len(github.listOpenPRCalls))
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.PRNumber == nil || *loop.PRNumber != 202 || loop.MetadataJSON == nil || !strings.Contains(*loop.MetadataJSON, `"prNumber":202`) {
		t.Fatalf("loop = %#v, want adopted PR persisted", loop)
	}
}

func TestProcessClaimedItemAdoptsLifecyclePRAndStampsMissingDisclosure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	branch := "looper/planner/42-plan-this"
	github := &fakeGitHubGateway{
		issues:      []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}},
		issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}},
		prDetail:    PullRequestDetail{Number: 202, URL: "https://example/pr/202", State: "OPEN", HeadRefName: branch, BaseRefName: "main", Body: "## Summary\n\nLifecycle-created body"},
	}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: branch, BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec", Lifecycle: &lifecycle.State{Branch: branch, BaseBranch: "main", PRNumber: 202, PRURL: "https://example/pr/202", Actions: lifecycle.Actions{PR: lifecycle.ActionSourceAgent}}}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "success" || result.PullRequestNumber != 202 {
		t.Fatalf("result = %#v, want success with adopted PR 202", result)
	}
	if len(github.updatePRBodyCalls) != 1 {
		t.Fatalf("updatePRBodyCalls = %#v, want one disclosure rewrite", github.updatePRBodyCalls)
	}
	if !strings.Contains(github.updatePRBodyCalls[0].Body, disclosure.Marker) {
		t.Fatalf("updated body = %q, want disclosure marker", github.updatePRBodyCalls[0].Body)
	}
	if !strings.Contains(github.updatePRBodyCalls[0].Body, "runner=planner") {
		t.Fatalf("updated body = %q, want planner disclosure footer", github.updatePRBodyCalls[0].Body)
	}
}

func TestProcessClaimedItemWriteSpecResumeDoesNotRerunAgentAfterTransientInspectFailure(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}, Body: "details", URL: "https://example/issues/42"}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}}
	git := &fakeGitGateway{
		createResult:  CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"},
		inspectErrors: []error{fmt.Errorf("temporary inspect failure")},
	}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	first, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first = %#v, want retryable_after_resume failure", first)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("agent starts after first attempt = %d, want 1", len(agent.starts))
	}

	fixture.advance(5 * time.Second)
	retryClaim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || retryClaim == nil {
		t.Fatalf("ClaimNextOfType(retry) = (%#v, %v), want claimed item", retryClaim, err)
	}
	second, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(second) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("second = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1", len(agent.starts))
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("len(git.pushCalls) = %d, want 1", len(git.pushCalls))
	}
	if len(github.createPRCalls) != 1 {
		t.Fatalf("len(github.createPRCalls) = %d, want 1", len(github.createPRCalls))
	}
}

func TestPublishResumeDoesNotRerunPriorSteps(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}, createPRResult: CreatePullRequestResult{Number: 101, URL: "https://example/pr/101"}, createPRErrors: []error{fmt.Errorf("temporary create pr failure")}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	firstClaim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	first, err := runner.ProcessClaimedItem(context.Background(), *firstClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(first) error = %v", err)
	}
	if first.Status != "failed" || first.FailureKind != FailureRetryableAfterResume {
		t.Fatalf("first result = %#v, want retryable_after_resume failure", first)
	}
	fixture.advance(5 * time.Second)
	retryClaim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	second, err := runner.ProcessClaimedItem(context.Background(), *retryClaim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem(retry) error = %v", err)
	}
	if second.Status != "success" {
		t.Fatalf("retry result = %#v, want success", second)
	}
	if len(agent.starts) != 1 {
		t.Fatalf("len(agent.starts) = %d, want 1 (write-spec not rerun)", len(agent.starts))
	}
	if len(git.pushCalls) != 1 {
		t.Fatalf("len(git.pushCalls) = %d, want 1 (push not rerun)", len(git.pushCalls))
	}
}

func TestValidatedLifecyclePullRequestTreatsLookupErrorAsNonAdoptable(t *testing.T) {
	t.Parallel()

	runner := New(Options{GitHub: &fakeGitHubGateway{viewPRErr: fmt.Errorf("not found")}})
	state := &lifecycle.State{PRNumber: 84, PRURL: "https://example/pr/84"}
	adopted, err := runner.validatedLifecyclePullRequest(context.Background(), stepInput{Project: storage.ProjectRecord{RepoPath: t.TempDir()}}, checkpointIssue{Repo: "acme/looper"}, checkpointWorktree{Branch: "looper/test", BaseBranch: "main"}, state)
	if err != nil {
		t.Fatalf("validatedLifecyclePullRequest() error = %v", err)
	}
	if adopted != nil {
		t.Fatalf("validatedLifecyclePullRequest() = %#v, want nil", adopted)
	}
}

func TestProcessClaimedItemResumeReleasesClaimedLockWhenSetupFails(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	issue := IssueSummary{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}
	loopResult, err := (&Runner{repos: fixture.repos, now: fixture.now}).ensureLoopForIssue(context.Background(), storage.ProjectRecord{ID: "project_1"}, "acme/looper", issue, buildPlannerDiscoveryFingerprint("acme/looper", fixture.now(), issue))
	if err != nil {
		t.Fatalf("ensureLoopForIssue() error = %v", err)
	}
	queueItem, err := (&Runner{repos: fixture.repos, now: fixture.now, retryMaxAttempts: 3}).enqueue(context.Background(), enqueueInput{ProjectID: "project_1", LoopID: loopResult.record.ID, Repo: "acme/looper", IssueNumber: issue.Number, Payload: map[string]any{"issueNumber": issue.Number}})
	if err != nil {
		t.Fatalf("enqueue() error = %v", err)
	}
	checkpointJSON := `{"claimedLockKey":"` + storage.IssueLockKey("project_1", "acme/looper", issue.Number) + `"}`
	if err := fixture.repos.Runs.Upsert(context.Background(), storage.RunRecord{ID: "run_failed_resume", LoopID: loopResult.record.ID, Status: "failed", LastCompletedStep: stringPtr(string(stepDiscoverIssues)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	if _, err := fixture.coordinator.DB().ExecContext(context.Background(), `
		CREATE TRIGGER loops_fail_running_resume_planner
		BEFORE UPDATE ON loops
		FOR EACH ROW
		WHEN NEW.id = '`+loopResult.record.ID+`' AND NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'forced loop update failure');
		END;
	`); err != nil {
		t.Fatalf("create trigger error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if claim.ID != queueItem.ID {
		t.Fatalf("claim.ID = %q, want %q", claim.ID, queueItem.ID)
	}
	_, err = runner.ProcessClaimedItem(context.Background(), *claim)
	if err == nil || !strings.Contains(err.Error(), "forced loop update failure") {
		t.Fatalf("ProcessClaimedItem() error = %v, want forced loop update failure", err)
	}
	lockKey := storage.IssueLockKey("project_1", "acme/looper", issue.Number)
	lock, err := fixture.repos.Locks.Get(context.Background(), lockKey)
	if err != nil {
		t.Fatalf("Locks.Get() error = %v", err)
	}
	if lock != nil {
		t.Fatalf("lock = %#v, want released claimed lock", lock)
	}
	acquired, err := fixture.repos.Locks.Acquire(context.Background(), storage.LockRecord{Key: lockKey, Owner: "retry", ExpiresAt: fixture.now().Add(time.Minute).UTC().Format("2006-01-02T15:04:05.000Z"), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()})
	if err != nil {
		t.Fatalf("Locks.Acquire() error = %v", err)
	}
	if !acquired {
		t.Fatal("Locks.Acquire() = false, want claimed lock to be immediately reacquirable")
	}
}

func TestWriteSpecFailureMarksRunQueueLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "agent failed"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient failure", result)
	}
	run, err := fixture.repos.Runs.GetByID(context.Background(), result.RunID)
	if err != nil {
		t.Fatalf("Runs.GetByID() error = %v", err)
	}
	if run == nil || run.Status != "failed" || run.CurrentStep == nil || *run.CurrentStep != string(stepWriteSpec) {
		t.Fatalf("run = %#v, want failed run on write-spec", run)
	}
	checkpoint := parseCheckpoint(run.CheckpointJSON)
	if checkpoint.ResumePolicy != "retry_from_timeout_context" {
		t.Fatalf("checkpoint.ResumePolicy = %q, want retry_from_timeout_context", checkpoint.ResumePolicy)
	}
	if checkpoint.WriteSpec == nil || checkpoint.WriteSpec.Status != "failed" || checkpoint.WriteSpec.Summary != "agent failed" {
		t.Fatalf("checkpoint.WriteSpec = %#v, want failed persisted write-spec checkpoint", checkpoint.WriteSpec)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued retry", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued for retry", loop)
	}
}

func TestWriteSpecSetupFailureStaysRetryable(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "failed", Summary: "unsupported model in agent configuration for codex"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient setup failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued retry", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "queued" {
		t.Fatalf("loop = %#v, want queued", loop)
	}
}

func TestPublishAutoPushDisabledPausesPlannerLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(false)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, _ := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureManualIntervention || !strings.Contains(result.Summary, "manual publish required") {
		t.Fatalf("result = %#v, want manual_intervention manual publish failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureManualIntervention) || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want parked manual_intervention item", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused parked loop", loop)
	}
}

func TestProcessClaimedItemPreservesPausedLoopOnRetryableFailureAfterPause(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	agent := &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}, wait: func(ctx context.Context) error {
		items, err := fixture.repos.Queue.List(ctx)
		if err != nil {
			return err
		}
		loopID := ""
		for _, item := range items {
			if item.Type == "planner" && item.Status == "running" && item.LoopID != nil {
				loopID = *item.LoopID
				break
			}
		}
		if loopID == "" {
			return fmt.Errorf("running planner queue item not found")
		}
		loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("loop not found: %s", loopID)
		}
		loop.Status = "paused"
		loop.NextRunAt = nil
		loop.UpdatedAt = fixture.nowISO()
		if err := fixture.repos.Loops.Upsert(ctx, *loop); err != nil {
			return err
		}
		reason := "loop paused"
		if _, err := fixture.repos.Queue.CancelByLoop(ctx, loopID, fixture.nowISO(), &reason); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: agent, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	if _, err := runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"}); err != nil {
		t.Fatalf("DiscoverIssues() error = %v", err)
	}
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	result, err := runner.ProcessClaimedItem(context.Background(), *claim)
	if err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	if result.Status != "failed" || result.FailureKind != FailureRetryableTransient {
		t.Fatalf("result = %#v, want retryable_transient failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), claim.ID)
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" {
		t.Fatalf("queue = %#v, want queued retry", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), result.LoopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused loop with nil next run", loop)
	}
}

func TestProcessNextSetupFailureMarksQueueFailed(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	projectID := "project_1"
	nowISO := fixture.nowISO()
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_missing_planner", ProjectID: &projectID, Type: "planner", TargetType: "issue", TargetID: "issue:acme/looper:99", DedupeKey: "planner:acme/looper:99", Priority: 1, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: &fakeGitHubGateway{}, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.ProcessNext(context.Background(), "planner-worker-1")
	if err != nil {
		t.Fatalf("ProcessNext() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable || !strings.Contains(result.Summary, "requires loopId") {
		t.Fatalf("result = %#v, want non-retryable missing-loopId failure", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_missing_planner")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "queued" || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureNonRetryable) || queue.FinishedAt != nil {
		t.Fatalf("queue = %#v, want requeued non_retryable item", queue)
	}
}

func TestRecoverClaimedItemReconcilesRunningLoopState(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	loopTarget := "issue:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(context.Background(), storage.LoopRecord{ID: "loop_planner_running", Seq: 1, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &loopTarget, Repo: stringPtr("acme/looper"), Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	projectID := "project_1"
	loopID := "loop_planner_running"
	payload := `{"issueNumber":42,"strictDispatchId":"dispatch-terminal"}`
	if err := fixture.repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{ID: "queue_planner_running", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	github := &fakeGitHubGateway{}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: &fakeGitGateway{}, AgentExecutor: &fakeAgentExecutor{}, Logger: fixture.logger, Now: fixture.now})

	result, err := runner.recoverClaimedItem(context.Background(), storage.QueueItemRecord{ID: "queue_planner_running", ProjectID: &projectID, LoopID: &loopID, Type: "planner", TargetType: "issue", TargetID: loopTarget, Repo: stringPtr("acme/looper"), DedupeKey: "planner:acme/looper:42", Priority: 1, Status: "running", AvailableAt: nowISO, Attempts: 0, MaxAttempts: 1, PayloadJSON: &payload, CreatedAt: nowISO, UpdatedAt: nowISO}, fmt.Errorf("persist step failed"))
	if err != nil {
		t.Fatalf("recoverClaimedItem() error = %v", err)
	}
	if result == nil || result.Status != "failed" || result.FailureKind != FailureNonRetryable {
		t.Fatalf("result = %#v, want failed non-retryable recovery", result)
	}
	queue, err := fixture.repos.Queue.GetByID(context.Background(), "queue_planner_running")
	if err != nil {
		t.Fatalf("Queue.GetByID() error = %v", err)
	}
	if queue == nil || queue.Status != "manual_intervention" || queue.LastErrorKind == nil || *queue.LastErrorKind != string(FailureNonRetryable) || queue.FinishedAt == nil {
		t.Fatalf("queue = %#v, want parked non_retryable recovery item", queue)
	}
	loop, err := fixture.repos.Loops.GetByID(context.Background(), loopID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if loop == nil || loop.Status != "paused" || loop.NextRunAt != nil {
		t.Fatalf("loop = %#v, want paused parked loop", loop)
	}
	if len(github.strictTransitions) != 1 || github.strictTransitions[0].DispatchID != "dispatch-terminal" || github.strictTransitions[0].State != "failed" {
		t.Fatalf("strictTransitions = %#v, want terminal failed transition", github.strictTransitions)
	}
}

func TestProcessClaimedItemUsesDefaultProjectWorktreeRootWhenProjectMetadataOmitsIt(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	project, err := fixture.repos.Projects.GetByID(context.Background(), "project_1")
	if err != nil {
		t.Fatalf("Projects.GetByID() error = %v", err)
	}
	if project == nil {
		t.Fatal("project missing")
	}
	metadata := `{"repo":"acme/looper"}`
	project.MetadataJSON = &metadata
	project.UpdatedAt = fixture.nowISO()
	if err := fixture.repos.Projects.Upsert(context.Background(), *project); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}

	github := &fakeGitHubGateway{issues: []IssueSummary{{Number: 42, Title: "Plan this", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}, issueDetail: IssueDetail{Number: 42, Title: "Plan this", Body: "details", URL: "https://example/issues/42", Assignees: []string{"octocat"}, Labels: []string{"looper:plan"}}}
	git := &fakeGitGateway{createResult: CreateWorktreeResult{ID: "worktree_1", WorktreePath: filepath.Join(t.TempDir(), "wt"), Branch: "looper/planner/42-plan-this", BaseBranch: "main"}}
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, GitHub: github, Git: git, AgentExecutor: &fakeAgentExecutor{results: []AgentResult{{Status: "completed", Summary: "wrote spec"}}}, Logger: fixture.logger, Now: fixture.now, AllowAutoPush: boolPtr(true)})

	_, _ = runner.DiscoverIssues(context.Background(), DiscoveryInput{ProjectID: "project_1", Repo: "acme/looper"})
	claim, err := fixture.repos.Queue.ClaimNextOfType(context.Background(), fixture.nowISO(), "planner-worker-1", "planner")
	if err != nil || claim == nil {
		t.Fatalf("ClaimNextOfType() = (%#v, %v), want claimed item", claim, err)
	}
	if _, err := runner.ProcessClaimedItem(context.Background(), *claim); err != nil {
		t.Fatalf("ProcessClaimedItem() error = %v", err)
	}
	wantRoot, err := config.DefaultProjectWorktreeRoot(project.ID, project.RepoPath)
	if err != nil {
		t.Fatalf("DefaultProjectWorktreeRoot() error = %v", err)
	}
	if len(git.createCalls) == 0 || git.createCalls[0].WorktreeRoot != wantRoot {
		t.Fatalf("CreateWorktree().WorktreeRoot = %#v, want %q", git.createCalls, wantRoot)
	}
}

type runnerFixture struct {
	coordinator *storage.SQLiteCoordinator
	repos       *storage.Repositories
	logger      *testLogger
	current     time.Time
	now         func() time.Time
}

func newRunnerFixture(t *testing.T) *runnerFixture {
	t.Helper()
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(t.TempDir(), "planner.sqlite"), storage.SQLiteCoordinatorOptions{BackupDir: t.TempDir()})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background()); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	now := time.Date(2026, time.April, 11, 12, 0, 0, 0, time.UTC)
	nowISO := fmt.Sprintf("%s.000Z", now.Format("2006-01-02T15:04:05"))
	baseBranch := "main"
	metadata := `{"repo":"acme/looper"}`
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{ID: "project_1", Name: "Looper", RepoPath: filepath.Join(t.TempDir(), "repo"), BaseBranch: &baseBranch, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	fixture := &runnerFixture{coordinator: coordinator, repos: repos, logger: &testLogger{}, current: now}
	fixture.now = func() time.Time { return fixture.current }
	return fixture
}

func (f *runnerFixture) advance(delta time.Duration) { f.current = f.current.Add(delta) }

func (f *runnerFixture) nowISO() string {
	return fmt.Sprintf("%s.000Z", f.current.UTC().Format("2006-01-02T15:04:05"))
}

type fakeGitHubGateway struct {
	issues             []IssueSummary
	listOpenIssueCalls []ListOpenIssuesInput
	issueDetail        IssueDetail
	issueDetails       []IssueDetail
	issueDetailIndex   int
	openPullRequests   []PullRequestSummary
	prDetail           PullRequestDetail
	viewPRErr          error
	createPRResult     CreatePullRequestResult
	createPRErrors     []error
	createPRIndex      int
	listOpenPRCalls    []ListOpenPullRequestsInput
	createPRCalls      []CreatePullRequestInput
	updatePRBodyCalls  []UpdatePullRequestBodyInput
	addLabelCalls      []PullRequestLabelsInput
	addReviewerCalls   []PullRequestReviewersInput
	closedPRs          []int64
	addAssigneeCalls   []IssueAssigneesInput
	addAssigneeErr     error
	login              string
	loginErr           error
	loginCalls         int
	strictTransitions  []StrictDispatchTransitionInput
}

func (f *fakeGitHubGateway) TransitionStrictDispatch(_ context.Context, input StrictDispatchTransitionInput) error {
	f.strictTransitions = append(f.strictTransitions, input)
	return nil
}

func (f *fakeGitHubGateway) CreateStrictRoleRequest(context.Context, StrictRoleRequestInput) (StrictRoleRequestResult, error) {
	return StrictRoleRequestResult{}, nil
}

func (f *fakeGitHubGateway) ListOpenIssues(_ context.Context, input ListOpenIssuesInput) ([]IssueSummary, error) {
	f.listOpenIssueCalls = append(f.listOpenIssueCalls, input)
	return append([]IssueSummary(nil), f.issues...), nil
}

func (f *fakeGitHubGateway) ViewIssue(_ context.Context, input ViewIssueInput) (IssueDetail, error) {
	detail := f.issueDetail
	if f.issueDetailIndex < len(f.issueDetails) {
		detail = f.issueDetails[f.issueDetailIndex]
		f.issueDetailIndex++
	}
	if detail.Number == 0 {
		detail.Number = input.IssueNumber
	}
	if detail.Title == "" {
		detail.Title = "Issue"
	}
	return detail, nil
}

func (f *fakeGitHubGateway) GetCurrentUserLogin(context.Context, string) (string, error) {
	f.loginCalls++
	if f.loginErr != nil {
		return "", f.loginErr
	}
	if f.login != "" {
		return f.login, nil
	}
	return "octocat", nil
}

func (f *fakeGitHubGateway) AddIssueAssignees(_ context.Context, input IssueAssigneesInput) error {
	f.addAssigneeCalls = append(f.addAssigneeCalls, input)
	return f.addAssigneeErr
}

func (f *fakeGitHubGateway) ListOpenPullRequests(_ context.Context, input ListOpenPullRequestsInput) ([]PullRequestSummary, error) {
	f.listOpenPRCalls = append(f.listOpenPRCalls, input)
	return append([]PullRequestSummary(nil), f.openPullRequests...), nil
}

func (f *fakeGitHubGateway) ViewPullRequest(_ context.Context, input ViewPullRequestInput) (PullRequestDetail, error) {
	if f.viewPRErr != nil {
		return PullRequestDetail{}, f.viewPRErr
	}
	detail := f.prDetail
	if detail.Number == 0 {
		detail.Number = input.PRNumber
	}
	return detail, nil
}

func (f *fakeGitHubGateway) CreatePullRequest(_ context.Context, input CreatePullRequestInput) (CreatePullRequestResult, error) {
	f.createPRCalls = append(f.createPRCalls, input)
	if f.createPRIndex < len(f.createPRErrors) && f.createPRErrors[f.createPRIndex] != nil {
		err := f.createPRErrors[f.createPRIndex]
		f.createPRIndex++
		return CreatePullRequestResult{}, err
	}
	f.createPRIndex++
	return f.createPRResult, nil
}

func (f *fakeGitHubGateway) UpdatePullRequestBody(_ context.Context, input UpdatePullRequestBodyInput) error {
	f.updatePRBodyCalls = append(f.updatePRBodyCalls, input)
	return nil
}

func (f *fakeGitHubGateway) AddPullRequestLabels(_ context.Context, input PullRequestLabelsInput) error {
	f.addLabelCalls = append(f.addLabelCalls, input)
	return nil
}

func (f *fakeGitHubGateway) AddPullRequestReviewers(_ context.Context, input PullRequestReviewersInput) error {
	f.addReviewerCalls = append(f.addReviewerCalls, input)
	return nil
}

func (f *fakeGitHubGateway) ClosePullRequest(_ context.Context, input ClosePullRequestInput) error {
	f.closedPRs = append(f.closedPRs, input.PRNumber)
	return nil
}

type fakeGitGateway struct {
	createResult  CreateWorktreeResult
	inspectResult InspectHeadResult
	inspectErrors []error
	inspectIndex  int
	commitResult  CommitResult
	createCalls   []CreateWorktreeInput
	inspectCalls  []InspectHeadInput
	commitCalls   []CommitInput
	pushCalls     []PushInput
}

func (f *fakeGitGateway) CreateWorktree(_ context.Context, input CreateWorktreeInput) (CreateWorktreeResult, error) {
	f.createCalls = append(f.createCalls, input)
	result := f.createResult
	if result.WorktreePath == "" {
		result.WorktreePath = filepath.Join(input.WorktreeRoot, "wt")
	} else if input.WorktreeRoot != "" {
		result.WorktreePath = filepath.Join(input.WorktreeRoot, filepath.Base(result.WorktreePath))
	}
	if result.Branch == "" {
		result.Branch = input.Branch
	}
	if result.BaseBranch == "" {
		result.BaseBranch = input.BaseBranch
	}
	if err := os.MkdirAll(result.WorktreePath, 0o755); err != nil {
		return CreateWorktreeResult{}, err
	}
	f.createResult = result
	return result, nil
}

func (f *fakeGitGateway) Push(_ context.Context, input PushInput) error {
	f.pushCalls = append(f.pushCalls, input)
	return nil
}

func (f *fakeGitGateway) InspectHead(_ context.Context, input InspectHeadInput) (InspectHeadResult, error) {
	f.inspectCalls = append(f.inspectCalls, input)
	if f.inspectIndex < len(f.inspectErrors) && f.inspectErrors[f.inspectIndex] != nil {
		err := f.inspectErrors[f.inspectIndex]
		f.inspectIndex++
		return InspectHeadResult{}, err
	}
	f.inspectIndex++
	return f.inspectResult, nil
}

func (f *fakeGitGateway) Commit(_ context.Context, input CommitInput) (CommitResult, error) {
	f.commitCalls = append(f.commitCalls, input)
	if f.commitResult.CommitSHA == "" {
		return CommitResult{CommitSHA: "fallback123"}, nil
	}
	return f.commitResult, nil
}

type fakeAgentExecutor struct {
	results []AgentResult
	starts  []AgentRunInput
	waitErr error
	wait    func(context.Context) error
}

func (f *fakeAgentExecutor) Start(_ context.Context, input AgentRunInput) (AgentExecution, error) {
	f.starts = append(f.starts, input)
	if len(f.results) == 0 {
		return nil, fmt.Errorf("no queued agent result")
	}
	result := f.results[0]
	f.results = f.results[1:]
	return fakeAgentExecution{result: result, waitErr: f.waitErr, wait: f.wait}, nil
}

type fakeAgentExecution struct {
	result  AgentResult
	waitErr error
	wait    func(context.Context) error
}

func (f fakeAgentExecution) Wait(ctx context.Context) (AgentResult, error) {
	if f.wait != nil {
		if err := f.wait(ctx); err != nil {
			return AgentResult{}, err
		}
	}
	if f.waitErr != nil {
		return AgentResult{}, f.waitErr
	}
	return f.result, nil
}

type testLogger struct{}

func (*testLogger) Debug(string, map[string]any) {}
func (*testLogger) Info(string, map[string]any)  {}
func (*testLogger) Warn(string, map[string]any)  {}
func (*testLogger) Error(string, map[string]any) {}

func boolPtr(value bool) *bool { return &value }

func TestUpdateLoopPreservesTerminatedLoop(t *testing.T) {
	fixture := newRunnerFixture(t)
	nowISO := fixture.nowISO()
	loop := storage.LoopRecord{ID: "loop_planner_terminated", Seq: 901, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "terminated", CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := fixture.repos.Loops.Upsert(context.Background(), loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}

	runner := &Runner{repos: fixture.repos, now: fixture.now}
	updated, err := runner.updateLoop(context.Background(), loop, func(current *storage.LoopRecord) {
		current.Status = "completed"
	})
	if err != nil {
		t.Fatalf("updateLoop() error = %v", err)
	}
	if updated.Status != "terminated" {
		t.Fatalf("updateLoop().Status = %q, want terminated", updated.Status)
	}

	persisted, err := fixture.repos.Loops.GetByID(context.Background(), loop.ID)
	if err != nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	if persisted == nil || persisted.Status != "terminated" {
		t.Fatalf("Loops.GetByID() = %#v, want terminated loop", persisted)
	}
}

// setupCompletedPlannerLoop seeds a finished planner loop and its successful run.
func setupCompletedPlannerLoop(t *testing.T, fixture *runnerFixture, loopID string, withSession bool) {
	t.Helper()
	ctx := context.Background()
	projectID := "project_1"
	target := "issue:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 42, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: stringPtr("acme/looper"), Status: "completed", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if withSession {
		if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "agent_planner_" + loopID, ProjectID: &projectID, LoopID: &loopID, Vendor: "opencode", Status: "completed", NativeSessionID: stringPtr("sess_planner_1"), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
			t.Fatalf("AgentExecutions.Upsert() error = %v", err)
		}
	}
	checkpointJSON := mustMarshalJSON(plannerCheckpoint{
		Issue:          &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md", URL: "https://plane/x/42"},
		ClaimedLockKey: "issue:acme/looper:42",
		Worktree:       &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/plan", BaseBranch: "main", SpecPath: "specs/42.md"},
		WriteSpec:      &checkpointWriteSpec{Status: "completed", GitReconciled: true},
		Publish:        &checkpointPublishState{PlaneSpecReview: true, Grilled: true, Reviewed: true},
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_planner_ok_" + loopID, LoopID: loopID, Status: "success", LastCompletedStep: stringPtr(string(stepNotify)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
}

func TestCreateRunContextInterruptedRunResumesFromCheckpoint(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	loopID := "loop_planner_interrupted"
	projectID := "project_1"
	target := "issue:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 43, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: stringPtr("acme/looper"), Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	if err := fixture.repos.AgentExecutions.Upsert(ctx, storage.AgentExecutionRecord{ID: "agent_" + loopID, ProjectID: &projectID, LoopID: &loopID, Vendor: "claude", Status: "killed", NativeSessionID: stringPtr("sess_1"), StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("AgentExecutions.Upsert() error = %v", err)
	}
	// Interrupted mid-grill: write-spec is done, grill/review not yet.
	checkpointJSON := mustMarshalJSON(plannerCheckpoint{
		Issue:          &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md", URL: "https://plane/x/42"},
		ClaimedLockKey: "issue:acme/looper:42",
		Worktree:       &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/plan", BaseBranch: "main", SpecPath: "specs/42.md"},
		WriteSpec:      &checkpointWriteSpec{Status: "completed", GitReconciled: true},
		Publish:        &checkpointPublishState{PlaneSpecReview: true, Grilled: false, Reviewed: false},
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "interrupted", LastCompletedStep: stringPtr(string(stepWriteSpec)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Runs.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	resumed, err := runner.createRunContext(ctx, *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if !resumed.Resumed || resumed.StartStep != stepPublish {
		t.Fatalf("resumed = {Resumed:%v StartStep:%v}, want checkpoint continuation", resumed.Resumed, resumed.StartStep)
	}
}

func TestCreateRunContextReopenedRequirementsResumesAtProductGrill(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	loopID := "loop_requirements_reopened"
	projectID := "project_1"
	target := "issue:acme/looper:42"
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 44, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: stringPtr("acme/looper"), Status: "queued", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	checkpointJSON := mustMarshalJSON(plannerCheckpoint{
		PipelineVersion: 2,
		Issue:           &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this"},
		Worktree:        &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/plan", BaseBranch: "main"},
		Decisions:       &decisions.State{Brief: decisions.Brief{Version: 1, Summary: "x"}, Stage: "requirements_reopened", ReopenReason: "RETURN_TO_REQUIREMENTS: missing boundary"},
		ResumePolicy:    loops.ResumePolicyManualIntervention,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "failed", LastCompletedStep: stringPtr(string(stepGrillFinalDecisions)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	resumed, err := runner.createRunContext(ctx, *loop)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.StartStep != stepGrillProductDecisions {
		t.Fatalf("resumed = {Resumed:%v StartStep:%v}", resumed.Resumed, resumed.StartStep)
	}
	if resumed.Checkpoint.Decisions == nil || resumed.Checkpoint.Decisions.ReopenReason == "" {
		t.Fatalf("reopen context was lost: %#v", resumed.Checkpoint.Decisions)
	}
}

func TestCreateRunContextV2WaitRequiresResolvedPlaneBarrier(t *testing.T) {
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	loopID := "loop_unresolved_v2_wait"
	target := "issue:acme/looper:46"
	metadata := `{"plannerPipelineVersion":2}`
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 46, ProjectID: "project_1", Type: "planner", TargetType: "issue", TargetID: &target, Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	checkpoint := plannerCheckpoint{PipelineVersion: 2, Wait: &checkpointPlannerWait{ResumeStep: stepGrillFinalDecisions}, Decisions: &decisions.State{Brief: decisions.Brief{Version: 1}, Stage: "awaiting_downstream"}}
	checkpointJSON := mustMarshalJSON(checkpoint)
	run := storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "success", LastCompletedStep: stringPtr(string(stepRouteDownstreamDecisions)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	loop, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	if _, err := runner.createRunContext(ctx, *loop); err == nil || !strings.Contains(err.Error(), "not resolved") {
		t.Fatalf("unresolved Plane barrier was accepted: %v", err)
	}
	checkpoint.Decisions.Stage = "downstream_resolved"
	checkpointJSON = mustMarshalJSON(checkpoint)
	run.CheckpointJSON = &checkpointJSON
	if err := fixture.repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	resumed, err := runner.createRunContext(ctx, *loop)
	if err != nil || !resumed.Resumed || resumed.StartStep != stepGrillFinalDecisions {
		t.Fatalf("resolved Plane barrier did not resume: %#v, %v", resumed, err)
	}
}

func TestCreateRunContextPromotesStaleV1CheckpointFromFrozenLoopMetadata(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	loopID := "loop_pipeline_promote"
	projectID := "project_1"
	target := "issue:acme/looper:45"
	metadata := `{"plannerPipelineVersion":2}`
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 45, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: stringPtr("acme/looper"), Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	checkpointJSON := mustMarshalJSON(plannerCheckpoint{PipelineVersion: 1})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "failed", CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v", err)
	}
	created, err := runner.createRunContext(ctx, *loop)
	if err != nil {
		t.Fatal(err)
	}
	if created.Checkpoint.PipelineVersion != 2 {
		t.Fatalf("PipelineVersion = %d, want frozen V2", created.Checkpoint.PipelineVersion)
	}
}

func TestCreateRunContextAnsweredPlaneDecisionResumesWriteSpec(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	ctx := context.Background()
	loopID := "loop_plane_answered"
	projectID := "project_1"
	ask := loops.HITLAsk{Question: "A or B?", Answer: "B", Status: "answered", Transport: "plane", SessionID: "sess-plane"}
	metadata, err := loops.WriteHITLAsk(nil, ask)
	if err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 44, ProjectID: projectID, Type: "planner", TargetType: "issue", Status: "queued", MetadataJSON: &metadata, CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	checkpointJSON := mustMarshalJSON(plannerCheckpoint{
		Issue:      &checkpointIssue{Repo: "acme/looper", IssueNumber: 42, Title: "Plan this", SpecPath: "specs/42.md", URL: "https://plane/x/42"},
		Worktree:   &checkpointWorktree{Path: filepath.Join(t.TempDir(), "wt"), Branch: "looper/plan", BaseBranch: "main", SpecPath: "specs/42.md"},
		WriteSpec:  &checkpointWriteSpec{Status: "completed", GitReconciled: true},
		Publish:    &checkpointPublishState{PlaneSpecReview: true, Grilled: true, Reviewed: true},
		Notify:     &checkpointNotify{SentAt: fixture.nowISO(), Message: "stale"},
		SkipReason: awaitingProductDecisionSkipReason,
	})
	if err := fixture.repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run_" + loopID, LoopID: loopID, Status: "success", LastCompletedStep: stringPtr(string(stepWriteSpec)), CheckpointJSON: &checkpointJSON, StartedAt: fixture.nowISO(), CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatal(err)
	}
	resumed, err := runner.createRunContext(ctx, loop)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Resumed || resumed.StartStep != stepWriteSpec || resumed.Checkpoint.WriteSpec != nil || resumed.Checkpoint.Publish != nil || resumed.Checkpoint.Notify != nil || resumed.Checkpoint.SkipReason != "" {
		t.Fatalf("resumed = %#v", resumed)
	}
	prompt, session := pendingPlaneDecisionAnswer(loop)
	if session != "sess-plane" || !strings.Contains(prompt, "B") {
		t.Fatalf("resume prompt/session = %q, %q", prompt, session)
	}
}

func TestCreateRunContextDoesNotResumeCompletedPlannerLoop(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	setupCompletedPlannerLoop(t, fixture, "loop_planner_followup", true)

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_planner_followup")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, loop)
	}
	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	if resumed.Resumed || resumed.StartStep != stepDiscoverIssues {
		t.Fatalf("resumed = {Resumed:%v StartStep:%v}, want completed loop to remain non-resumable", resumed.Resumed, resumed.StartStep)
	}
}

func TestCreateRunContextDoesNotFollowupResumePlannerWithoutNativeSession(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	runner := New(Options{DB: fixture.coordinator.DB(), Repos: fixture.repos, Logger: fixture.logger, Now: fixture.now})
	setupCompletedPlannerLoop(t, fixture, "loop_planner_nosession", false) // no captured session

	loop, err := fixture.repos.Loops.GetByID(context.Background(), "loop_planner_nosession")
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, loop)
	}
	resumed, err := runner.createRunContext(context.Background(), *loop)
	if err != nil {
		t.Fatalf("createRunContext() error = %v", err)
	}
	// Without a native session, native resume is impossible — the follow-up bridge
	// must refuse (a completed run is not otherwise resumed), so no re-plan is forced.
	if resumed.Resumed || resumed.StartStep != stepDiscoverIssues {
		t.Fatalf("resumed = {Resumed:%v StartStep:%v}, want NO follow-up resume (fresh discover-issues)", resumed.Resumed, resumed.StartStep)
	}
}

func TestSetAwaitingProductAnswerMarker(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	r := &Runner{repos: fixture.repos, now: fixture.now, logger: fixture.logger}
	ctx := context.Background()
	loopID := "loop_product_answer"
	if err := fixture.repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 7, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	loop, err := fixture.repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		t.Fatalf("Loops.GetByID() error = %v, loop = %v", err, loop)
	}

	r.setAwaitingProductAnswerMarker(ctx, *loop, true)
	after, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	if v, _ := parseJSONObject(after.MetadataJSON)["awaitingProductAnswer"].(bool); !v {
		t.Fatalf("awaitingProductAnswer = false, want true; meta=%q", derefString(after.MetadataJSON))
	}

	r.setAwaitingProductAnswerMarker(ctx, *after, false)
	cleared, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	if v, _ := parseJSONObject(cleared.MetadataJSON)["awaitingProductAnswer"].(bool); v {
		t.Fatalf("awaitingProductAnswer = true after clear, want false; meta=%q", derefString(cleared.MetadataJSON))
	}
}

func TestSetAwaitingProductSpecMarkerPreservesFreshCardMessageID(t *testing.T) {
	t.Parallel()
	fixture := newRunnerFixture(t)
	r := &Runner{repos: fixture.repos, now: fixture.now, logger: fixture.logger}
	ctx := context.Background()
	loopID := "loop_product_spec_card"
	loop := storage.LoopRecord{ID: loopID, Seq: 8, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	stale, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	fresh, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	cardMetadata := `{"productAskCardMsgId":"om_product_spec"}`
	fresh.MetadataJSON = &cardMetadata
	if err := fixture.repos.Loops.Upsert(ctx, *fresh); err != nil {
		t.Fatal(err)
	}

	r.setAwaitingProductSpecMarker(ctx, *stale, true, "comment-product", &checkpointIssue{IssueNumber: 582, Title: "导出", URL: "https://plane.test/issues/582"})
	after, _ := fixture.repos.Loops.GetByID(ctx, loopID)
	metadata := parseJSONObject(after.MetadataJSON)
	if metadata["productAskCardMsgId"] != "om_product_spec" || metadata["awaitingProductSpec"] != true {
		t.Fatalf("metadata = %s; want fresh card id preserved with product-spec hold", derefString(after.MetadataJSON))
	}
	condition, ok := loopcondition.Read(after.MetadataJSON)
	if !ok || condition.Kind != loopcondition.ProductSpec || condition.Fingerprint != "comment-product" {
		t.Fatalf("condition = %#v, present=%v", condition, ok)
	}
}

func TestNotifyProductSpecClarificationUsesExactPlaneURLAndMentionsProductOwner(t *testing.T) {
	t.Parallel()
	var gotLoopID, gotText, gotActionURL string
	var gotMentions []string
	roleCfg := &config.Config{Projects: []config.ProjectRefConfig{{ID: "project_1", ProductOwner: &config.ProductOwnerConfig{FeishuOpenID: "ou_sunqingyu"}}}}
	r := &Runner{
		logger:            &testLogger{},
		projectRoleConfig: roleCfg,
		postThreadProductSpecCard: func(_ context.Context, loopID, text, actionURL string, mentions []string) error {
			gotLoopID, gotText, gotActionURL, gotMentions = loopID, text, actionURL, mentions
			return nil
		},
	}
	in := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: storage.LoopRecord{ID: "loop_x"}}
	r.notifyProductSpecClarification(context.Background(), in, "① 背景:客户 A 要导出。\n③ 问题:先做 A 还是 B?建议 A。", "https://plane.test/issues/wi-1#comment-c-1")

	if gotLoopID != "loop_x" {
		t.Fatalf("loopID = %q, want loop_x", gotLoopID)
	}
	if !strings.Contains(gotText, "① 背景") || !strings.Contains(gotText, "建议 A") {
		t.Fatalf("text = %q, want the productAsk embedded", gotText)
	}
	if gotActionURL != "https://plane.test/issues/wi-1#comment-c-1" {
		t.Fatalf("actionURL = %q, want exact Plane comment URL", gotActionURL)
	}
	if len(gotMentions) != 1 || gotMentions[0] != "ou_sunqingyu" {
		t.Fatalf("mentions = %v, want [ou_sunqingyu] (@ product owner)", gotMentions)
	}
}

func TestRequestProductSpecClarificationReturnsToWorkItem(t *testing.T) {
	t.Parallel()

	fixture := newRunnerFixture(t)
	ctx := context.Background()
	loop := storage.LoopRecord{ID: "loop_product_clarification", Seq: 82, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "running", CreatedAt: fixture.nowISO(), UpdatedAt: fixture.nowISO()}
	if err := fixture.repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	gw, calls := scriptedGateway(`{"id":"comment-product-spec"}`)
	var notifiedURL string
	r := &Runner{
		repos: fixture.repos,
		now:   fixture.now,
		planeDoc: func(string) (*planedoc.Gateway, string, bool) {
			return gw, "plane-project", true
		},
		postThreadProductSpecCard: func(_ context.Context, _, _, actionURL string, _ []string) error {
			notifiedURL = actionURL
			return nil
		},
	}
	in := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: loop}
	checkpoint := plannerCheckpoint{Issue: &checkpointIssue{URL: "https://plane.test/open-design/projects/p/issues/work-item-582"}}

	actionURL, err := r.requestProductSpecClarificationOnPlane(ctx, in, checkpoint, "请在产品 spec 明确首版范围")
	if err != nil {
		t.Fatalf("requestProductSpecClarificationOnPlane() error = %v", err)
	}
	if actionURL != "https://plane.test/open-design/projects/p/issues/work-item-582#comment-comment-product-spec" || notifiedURL != actionURL {
		t.Fatalf("action URLs = %q, %q; want exact work-item comment", actionURL, notifiedURL)
	}
	joinedCall := ""
	if len(*calls) == 1 {
		joinedCall = strings.Join((*calls)[0], " ")
	}
	if len(*calls) != 1 || !strings.Contains(joinedCall, "api request workspaces/w/projects/plane-project/work-items/work-item-582/comments/ --method POST") || strings.Contains(joinedCall, " page ") {
		t.Fatalf("Plane calls = %v, want one work-item comment and no tech-page publish", *calls)
	}
	if !strings.Contains(joinedCall, "产品 spec 需要补充") {
		t.Fatalf("comment call missing product-spec guidance: %v", (*calls)[0])
	}
}

func TestNotifySpecApprovalMentionsLooperOwner(t *testing.T) {
	t.Parallel()

	var gotActionURL string
	var gotMentions []string
	roleCfg := &config.Config{Projects: []config.ProjectRefConfig{{
		ID:           "project_1",
		ProductOwner: &config.ProductOwnerConfig{FeishuOpenID: "ou_product"},
		Owner:        &config.FeishuActorConfig{FeishuOpenID: "ou_looper_owner"},
	}}}
	r := &Runner{
		logger:            &testLogger{},
		projectRoleConfig: roleCfg,
		postThreadApprovalCard: func(_ context.Context, _, _, actionURL string, mentions []string) error {
			gotActionURL, gotMentions = actionURL, mentions
			return nil
		},
	}
	in := stepInput{Project: storage.ProjectRecord{ID: "project_1"}, Loop: storage.LoopRecord{ID: "loop_x"}}
	r.notifySpecApproval(context.Background(), in, "请审核技术方案", "https://plane.test/pages/spec#comment-review")

	if gotActionURL != "https://plane.test/pages/spec#comment-review" {
		t.Fatalf("actionURL = %q, want exact review comment URL", gotActionURL)
	}
	if len(gotMentions) != 1 || gotMentions[0] != "ou_looper_owner" {
		t.Fatalf("mentions = %v, want [ou_looper_owner], not product owner", gotMentions)
	}
}
