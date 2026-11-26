package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/planner/decisions"
	"github.com/nexu-io/looper/internal/storage"
)

func TestConsumeDecisionAnswersEnforcesRoleAndRevision(t *testing.T) {
	state := decisions.State{
		Brief:    decisions.Brief{Revision: 2, Questions: []decisions.Question{{ID: "DESIGN-001", Role: decisions.RoleDesign, Blocking: true, Options: []decisions.Option{{ID: "DESIGN-001-A"}, {ID: "DESIGN-001-B"}}}}},
		Requests: map[decisions.Role]decisions.RequestReceipt{decisions.RoleDesign: {Role: decisions.RoleDesign, Revision: 2, CommentID: "request", CreatedAt: "2026-07-17T10:00:00Z"}},
	}
	comments := []planedoc.WorkItemComment{
		{ID: "old", Actor: "designer", CreatedAt: "2026-07-17T09:00:00Z", CommentHTML: "DESIGN-001: DESIGN-001-A"},
		{ID: "wrong", Actor: "product", CreatedAt: "2026-07-17T10:01:00Z", CommentHTML: "DESIGN-001: DESIGN-001-A"},
		{ID: "latest", Actor: "designer", CreatedAt: "2026-07-17T10:03:00Z", CommentHTML: "<p>DESIGN-001: DESIGN-001-B</p>"},
		{ID: "right", Actor: "designer", CreatedAt: "2026-07-17T10:02:00Z", CommentHTML: "<p>DESIGN-001: DESIGN-001-A</p>"},
	}
	consumeDecisionAnswers(&state, decisions.RoleDesign, "designer", comments)
	if got := state.Answers["DESIGN-001"]; got.Value != "DESIGN-001-B" || got.CommentID != "latest" || got.Revision != 2 || got.QuestionHash != decisions.QuestionHash(state.Brief.Questions[0]) {
		t.Fatalf("answer = %#v", got)
	}
}

func TestConsumeDecisionAnswersClearsEarlierAnswerOnNewConflictingReply(t *testing.T) {
	question := decisions.Question{ID: "DESIGN-001", Role: decisions.RoleDesign, Blocking: true, Options: []decisions.Option{{ID: "DESIGN-001-A"}, {ID: "DESIGN-001-B"}}}
	state := decisions.State{
		Brief:    decisions.Brief{Revision: 2, Questions: []decisions.Question{question}},
		Requests: map[decisions.Role]decisions.RequestReceipt{decisions.RoleDesign: {Role: decisions.RoleDesign, Revision: 2, CommentID: "request", CreatedAt: "2026-07-17T10:00:00Z"}},
		Answers:  map[string]decisions.Answer{"DESIGN-001": {QuestionID: "DESIGN-001", Value: "DESIGN-001-A", Revision: 2, QuestionHash: decisions.QuestionHash(question)}},
	}
	consumeDecisionAnswers(&state, decisions.RoleDesign, "designer", []planedoc.WorkItemComment{{ID: "conflict", Actor: "designer", CreatedAt: "2026-07-17T10:02:00Z", CommentHTML: "DESIGN-001: DESIGN-001-A<br>DESIGN-001: DESIGN-001-B"}})
	if _, exists := state.Answers["DESIGN-001"]; exists {
		t.Fatalf("conflicting newer comment left stale answer: %#v", state.Answers)
	}
	if unanswered := decisions.UnansweredBlocking(state, decisions.RoleDesign); len(unanswered) != 1 || unanswered[0].ID != "DESIGN-001" {
		t.Fatalf("conflicting newer comment did not restore barrier: %#v", unanswered)
	}
}

func TestWorkItemCommentTextPrefersVisibleHTMLOverDescriptionUUID(t *testing.T) {
	comment := planedoc.WorkItemComment{
		Description: "0cf811a7-afee-4afc-a7c7-0bfe8198d055",
		CommentHTML: "<p>PROD-001: PROD-001-A</p><p>采用手动重试。</p>",
	}
	if got := workItemCommentText(comment); got != "PROD-001: PROD-001-A\n采用手动重试。" {
		t.Fatalf("workItemCommentText() = %q", got)
	}
}

func TestPersistAndQueueResolvedDecisionRepairsCrashWindowIdempotently(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-17T10:00:00.000Z"
	resumeAt := "2026-07-17T10:00:01.000Z"
	projectID, loopID, target, repo := "project", "loop", "issue:acme/looper:1593", "acme/looper"
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	metadata := `{"plannerPipelineVersion":2,"decisionPhase":"downstream_resolved"}`
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	checkpoint := `{"plannerPipelineVersion":2,"phase":"downstream_resolved","issue":{"url":"https://plane.example/issues/1593"},"decisions":{"stage":"downstream_resolved"}}`
	run := storage.RunRecord{ID: "run", LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repos.Runs.Upsert(ctx, run); err != nil {
		t.Fatal(err)
	}
	view := decisionCheckpointView{PipelineVersion: 2, Phase: "downstream_resolved", Decisions: &decisions.State{Stage: "downstream_resolved"}}
	if err := persistAndQueueResolvedDecision(ctx, repos, loop, run, view, nowISO, resumeAt, 3); err != nil {
		t.Fatal(err)
	}
	first, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || first == nil || first.Status != "queued" {
		t.Fatalf("first active queue = %#v, err=%v", first, err)
	}
	// A reconciler retry after a crash/restart must converge on the same active item,
	// not create parallel planner executions.
	if err := persistAndQueueResolvedDecision(ctx, repos, loop, run, view, nowISO, resumeAt, 3); err != nil {
		t.Fatal(err)
	}
	second, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || second == nil || second.ID != first.ID {
		t.Fatalf("second active queue = %#v, first=%#v, err=%v", second, first, err)
	}
	gotLoop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || gotLoop == nil || gotLoop.Status != "queued" || gotLoop.NextRunAt == nil || *gotLoop.NextRunAt != resumeAt {
		t.Fatalf("loop = %#v, err=%v", gotLoop, err)
	}
	gotRun, err := repos.Runs.GetByID(ctx, "run")
	if err != nil || gotRun == nil || gotRun.CheckpointJSON == nil {
		t.Fatalf("run = %#v, err=%v", gotRun, err)
	}
	var persisted decisionCheckpointView
	if json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted) != nil || persisted.Decisions == nil || persisted.Decisions.Stage != "downstream_resolved" {
		t.Fatalf("checkpoint = %s", *gotRun.CheckpointJSON)
	}
}

func TestRoleDecisionReconcilerWaitsForBothDownstreamRolesAndQueuesOnce(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 10, 5, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, repo, target := "project", "loop-reconcile", "acme/looper", "issue:acme/looper:1593"
	designQuestion := decisions.Question{ID: "DESIGN-001", Role: decisions.RoleDesign, Blocking: true, Question: "按钮放哪里？", Context: "导出页需要新按钮。", Options: []decisions.Option{{ID: "DESIGN-001-A", Label: "顶部"}, {ID: "DESIGN-001-B", Label: "底部"}}}
	engQuestion := decisions.Question{ID: "ENG-001", Role: decisions.RoleEngineering, Blocking: true, Question: "是否异步？", Context: "大文件会超时。", Options: []decisions.Option{{ID: "ENG-001-A", Label: "异步"}, {ID: "ENG-001-B", Label: "同步"}}}
	state := &decisions.State{
		Brief: decisions.Brief{Version: 1, Revision: 4, Summary: "导出", Questions: []decisions.Question{designQuestion, engQuestion}},
		Stage: "awaiting_downstream",
		Requests: map[decisions.Role]decisions.RequestReceipt{
			decisions.RoleDesign:      {Role: decisions.RoleDesign, Revision: 4, CommentID: "ask-design", CreatedAt: "2026-07-17T10:00:00Z"},
			decisions.RoleEngineering: {Role: decisions.RoleEngineering, Revision: 4, CommentID: "ask-eng", CreatedAt: "2026-07-17T10:00:00Z"},
		},
		RequestedQuestions: map[decisions.Role][]decisions.Question{decisions.RoleDesign: {designQuestion}, decisions.RoleEngineering: {engQuestion}},
		Answers:            map[string]decisions.Answer{},
	}
	view := decisionCheckpointView{PipelineVersion: 2, Phase: state.Stage, Decisions: state, Issue: &struct {
		URL string `json:"url"`
	}{URL: "https://plane.example/workspace/projects/project/issues/wi-1593"}}
	checkpointBytes, _ := json.Marshal(view)
	checkpoint := string(checkpointBytes)
	metadata := `{"plannerPipelineVersion":2,"decisionPhase":"awaiting_downstream"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run-reconcile", LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}

	comments := []planedoc.WorkItemComment{{ID: "design-answer", Actor: "designer", CreatedAt: "2026-07-17T10:01:00Z", CommentHTML: "<p>DESIGN-001: DESIGN-001-A</p>"}}
	postCount := 0
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		if strings.Contains(joined, "--method\x00GET") && strings.Contains(joined, "/comments/") {
			encoded, _ := json.Marshal(map[string]any{"results": comments})
			return shell.Result{Stdout: string(encoded)}, nil
		}
		if strings.Contains(joined, "--method\x00POST") && strings.Contains(joined, "/comments/") {
			postCount++
			commentHTML := ""
			for i := range options.Args {
				if options.Args[i] == "--data" && i+1 < len(options.Args) {
					var body map[string]string
					_ = json.Unmarshal([]byte(options.Args[i+1]), &body)
					commentHTML = body["comment_html"]
				}
			}
			created := planedoc.WorkItemComment{ID: "decision-log", Actor: "looper", CreatedAt: "2026-07-17T10:05:00Z", CommentHTML: commentHTML}
			comments = append(comments, created)
			encoded, _ := json.Marshal(created)
			return shell.Result{Stdout: string(encoded)}, nil
		}
		return shell.Result{}, nil
	}})
	cfg := config.Config{Scheduler: config.SchedulerConfig{RetryMaxAttempts: 3}, Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), ProductOwner: &config.ProductOwnerConfig{PlaneID: "product"}, DesignOwner: &config.FeishuActorConfig{PlaneID: "designer"}, Owner: &config.FeishuActorConfig{PlaneID: "engineer"}}}}
	runtime := &Runtime{config: cfg, now: func() time.Time { return now }, services: Services{Repositories: repos}, planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true }}

	runtime.reconcileAwaitingRoleDecisions(ctx)
	afterDesign, _ := repos.Loops.GetByID(ctx, loopID)
	active, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if afterDesign == nil || afterDesign.Status != "paused" || active != nil {
		t.Fatalf("partial design answer must remain paused: loop=%#v queue=%#v", afterDesign, active)
	}
	partialRun, _ := repos.Runs.GetByID(ctx, "run-reconcile")
	var partial decisionCheckpointView
	if partialRun == nil || partialRun.CheckpointJSON == nil || json.Unmarshal([]byte(*partialRun.CheckpointJSON), &partial) != nil || partial.Decisions == nil || partial.Decisions.Answers["DESIGN-001"].Value != "DESIGN-001-A" {
		t.Fatalf("partial design answer was not durably checkpointed: %#v", partialRun)
	}

	comments = append(comments, planedoc.WorkItemComment{ID: "eng-answer", Actor: "engineer", CreatedAt: "2026-07-17T10:02:00Z", CommentHTML: "<p>ENG-001: ENG-001-A</p>"})
	runtime.reconcileAwaitingRoleDecisions(ctx)
	afterResolved, _ := repos.Loops.GetByID(ctx, loopID)
	active, _ = repos.Queue.FindActiveByLoopID(ctx, loopID)
	if afterResolved == nil || afterResolved.Status != "paused" || active != nil {
		t.Fatalf("first complete snapshot must remain paused for one authority recheck: loop=%#v queue=%#v", afterResolved, active)
	}
	runtime.reconcileAwaitingRoleDecisions(ctx)
	afterBoth, _ := repos.Loops.GetByID(ctx, loopID)
	first, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if afterBoth == nil || afterBoth.Status != "queued" || first == nil {
		t.Fatalf("both answers must resume exactly one queue: loop=%#v queue=%#v", afterBoth, first)
	}
	runtime.reconcileAwaitingRoleDecisions(ctx)
	second, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if second == nil || second.ID != first.ID || postCount != 2 {
		t.Fatalf("repeat tick duplicated work: first=%#v second=%#v decisionLogs=%d", first, second, postCount)
	}
}

func TestRoleDecisionReconcilerReopensQueuedResolvedBarrierOnLateProductConflict(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 11, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, repo, target := "project-conflict", "loop-conflict", "acme/looper", "issue:acme/looper:1594"
	productQuestion := decisions.Question{ID: "PROD-001", Role: decisions.RoleProduct, Blocking: true, Question: "重试策略？", Context: "导出失败需要恢复。", Options: []decisions.Option{{ID: "PROD-001-A", Label: "自动"}, {ID: "PROD-001-B", Label: "手动"}}}
	state := &decisions.State{
		// A real downstream fresh GRILL removes the already-answered product
		// question from the current Brief; its authority survives in RequestedQuestions.
		Brief:              decisions.Brief{Version: 1, Revision: 5, Summary: "导出重试", Questions: nil},
		Stage:              "downstream_resolved",
		Requests:           map[decisions.Role]decisions.RequestReceipt{decisions.RoleProduct: {Role: decisions.RoleProduct, Revision: 5, CommentID: "ask-product", CreatedAt: "2026-07-17T10:00:00Z"}},
		RequestedQuestions: map[decisions.Role][]decisions.Question{decisions.RoleProduct: {productQuestion}},
		Answers:            map[string]decisions.Answer{"PROD-001": {QuestionID: "PROD-001", Value: "PROD-001-A", Revision: 5, QuestionHash: decisions.QuestionHash(productQuestion), CommentID: "old", Actor: "product", CreatedAt: "2026-07-17T10:01:00Z"}},
	}
	view := decisionCheckpointView{PipelineVersion: 2, Phase: state.Stage, Decisions: state, Issue: &struct {
		URL string `json:"url"`
	}{URL: "https://plane.example/workspace/projects/project-conflict/issues/wi-1594"}}
	checkpointBytes, _ := json.Marshal(view)
	checkpoint := string(checkpointBytes)
	metadata := `{"plannerPipelineVersion":2,"decisionPhase":"downstream_resolved"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 3, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	projectIDRef, loopIDRef := projectID, loopID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue-conflict", ProjectID: &projectIDRef, LoopID: &loopIDRef, Type: "planner", TargetType: "issue", TargetID: target, Repo: &repo, DedupeKey: "planner:" + projectID + ":" + loopID, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run-conflict", LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	comments := []planedoc.WorkItemComment{{ID: "conflicting-product", Actor: "product", CreatedAt: "2026-07-17T10:02:00Z", CommentHTML: "PROD-001: PROD-001-X"}}
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		if strings.Contains(joined, "--method\x00GET") && strings.Contains(joined, "/comments/") {
			encoded, _ := json.Marshal(map[string]any{"results": comments})
			return shell.Result{Stdout: string(encoded)}, nil
		}
		if strings.Contains(joined, "--method\x00POST") && strings.Contains(joined, "/comments/") {
			return shell.Result{Stdout: `{"id":"log-conflict","actor":"looper","created_at":"2026-07-17T11:00:00Z"}`}, nil
		}
		return shell.Result{}, nil
	}})
	cfg := config.Config{Scheduler: config.SchedulerConfig{RetryMaxAttempts: 3}, Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), ProductOwner: &config.ProductOwnerConfig{PlaneID: "product"}}}}
	runtime := &Runtime{config: cfg, now: func() time.Time { return now }, services: Services{Repositories: repos}, planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true }}
	runtime.reconcileAwaitingRoleDecisions(ctx)
	gotLoop, _ := repos.Loops.GetByID(ctx, loopID)
	active, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	gotRun, _ := repos.Runs.GetByID(ctx, "run-conflict")
	var persisted decisionCheckpointView
	if gotRun == nil || gotRun.CheckpointJSON == nil || json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted) != nil {
		t.Fatalf("run = %#v", gotRun)
	}
	if gotLoop == nil || gotLoop.Status != "paused" || active != nil || persisted.Decisions == nil || persisted.Decisions.Stage != "awaiting_product" || len(decisions.UnansweredBlocking(*persisted.Decisions, decisions.RoleProduct)) != 1 {
		t.Fatalf("conflicting product replacement bypassed barrier: loop=%#v queue=%#v checkpoint=%#v", gotLoop, active, persisted.Decisions)
	}
}

func TestRoleDecisionReconcilerRepairsPartialReopenWritesWithoutNewComment(t *testing.T) {
	for _, initialLoopStatus := range []string{"queued", "paused"} {
		t.Run(initialLoopStatus, func(t *testing.T) {
			repos := newEnqueueTestRepos(t)
			ctx := context.Background()
			now := time.Date(2026, 7, 17, 12, 30, 0, 0, time.UTC)
			nowISO := formatJavaScriptISOString(now)
			projectID, loopID, repo, target := "project-repair-"+initialLoopStatus, "loop-repair-"+initialLoopStatus, "acme/looper", "issue:acme/looper:1600"
			question := decisions.Question{ID: "DESIGN-001", Role: decisions.RoleDesign, Blocking: true, Question: "位置？", Context: "完整背景", Options: []decisions.Option{{ID: "DESIGN-001-A"}, {ID: "DESIGN-001-B"}}}
			state := &decisions.State{
				Brief:              decisions.Brief{Version: 1, Revision: 2, Questions: []decisions.Question{question}},
				Stage:              "awaiting_downstream",
				Requests:           map[decisions.Role]decisions.RequestReceipt{decisions.RoleDesign: {Role: decisions.RoleDesign, Revision: 2, CommentID: "ask", CreatedAt: "2026-07-17T12:00:00Z"}},
				RequestedQuestions: map[decisions.Role][]decisions.Question{decisions.RoleDesign: {question}},
			}
			view := decisionCheckpointView{PipelineVersion: 2, Phase: state.Stage, Decisions: state, Issue: &struct {
				URL string `json:"url"`
			}{URL: "https://plane.example/workspace/projects/" + projectID + "/issues/wi-1600"}}
			checkpointBytes, _ := json.Marshal(view)
			checkpoint := string(checkpointBytes)
			metadata := `{"plannerPipelineVersion":2,"decisionPhase":"awaiting_downstream"}`
			if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 4, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: initialLoopStatus, MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatal(err)
			}
			if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run-" + loopID, LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatal(err)
			}
			projectIDRef, loopIDRef := projectID, loopID
			if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue-" + loopID, ProjectID: &projectIDRef, LoopID: &loopIDRef, Type: "planner", TargetType: "issue", TargetID: target, Repo: &repo, DedupeKey: "planner:" + projectID + ":" + loopID, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
				t.Fatal(err)
			}
			gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
				if strings.Contains(strings.Join(options.Args, "\x00"), "/comments/") {
					return shell.Result{Stdout: `{"results":[]}`}, nil
				}
				return shell.Result{}, nil
			}})
			cfg := config.Config{Scheduler: config.SchedulerConfig{RetryMaxAttempts: 3}, Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), DesignOwner: &config.FeishuActorConfig{PlaneID: "designer"}}}}
			runtime := &Runtime{config: cfg, now: func() time.Time { return now }, services: Services{Repositories: repos}, planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true }}

			runtime.reconcileAwaitingRoleDecisions(ctx)
			gotLoop, _ := repos.Loops.GetByID(ctx, loopID)
			active, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
			if gotLoop == nil || gotLoop.Status != "paused" || active != nil {
				t.Fatalf("partial reopen did not converge: loop=%#v active=%#v", gotLoop, active)
			}
		})
	}
}

func TestRoleDecisionReconcilerParksQueuedLateValidProductReplacement(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, repo, target := "project-late-valid", "loop-late-valid", "acme/looper", "issue:acme/looper:1601"
	productQuestion := decisions.Question{ID: "PROD-001", Role: decisions.RoleProduct, Blocking: true, Question: "重试策略？", Context: "导出失败需要恢复。", Options: []decisions.Option{{ID: "PROD-001-A"}, {ID: "PROD-001-B"}}}
	designQuestion := decisions.Question{ID: "DESIGN-001", Role: decisions.RoleDesign, Blocking: true, Question: "按钮位置？", Context: "完整背景", Options: []decisions.Option{{ID: "DESIGN-001-A"}, {ID: "DESIGN-001-B"}}}
	state := &decisions.State{
		Brief: decisions.Brief{Version: 1, Revision: 6, Summary: "导出", Questions: []decisions.Question{designQuestion}},
		Stage: "downstream_resolved",
		Requests: map[decisions.Role]decisions.RequestReceipt{
			decisions.RoleProduct: {Role: decisions.RoleProduct, Revision: 5, CommentID: "ask-product", CreatedAt: "2026-07-17T12:00:00Z"},
			decisions.RoleDesign:  {Role: decisions.RoleDesign, Revision: 6, CommentID: "ask-design", CreatedAt: "2026-07-17T12:10:00Z"},
		},
		RequestedQuestions: map[decisions.Role][]decisions.Question{decisions.RoleProduct: {productQuestion}, decisions.RoleDesign: {designQuestion}},
		Answers: map[string]decisions.Answer{
			"PROD-001":   {QuestionID: "PROD-001", Value: "PROD-001-A", Revision: 5, QuestionHash: decisions.QuestionHash(productQuestion), Actor: "product"},
			"DESIGN-001": {QuestionID: "DESIGN-001", Value: "DESIGN-001-A", Revision: 6, QuestionHash: decisions.QuestionHash(designQuestion), Actor: "designer"},
		},
		ImageMessages: map[string]string{"DESIGN-001-A": "image"},
	}
	view := decisionCheckpointView{PipelineVersion: 2, Phase: state.Stage, Decisions: state, Issue: &struct {
		URL string `json:"url"`
	}{URL: "https://plane.example/workspace/projects/project-late-valid/issues/wi-1601"}}
	checkpointBytes, _ := json.Marshal(view)
	checkpoint := string(checkpointBytes)
	metadata := `{"plannerPipelineVersion":2,"decisionPhase":"downstream_resolved"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 5, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run-late-valid", LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	projectIDRef, loopIDRef := projectID, loopID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue-late-valid", ProjectID: &projectIDRef, LoopID: &loopIDRef, Type: "planner", TargetType: "issue", TargetID: target, Repo: &repo, DedupeKey: "planner:" + projectID + ":" + loopID, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	comments := []planedoc.WorkItemComment{{ID: "late-product", Actor: "product", CreatedAt: "2026-07-17T12:20:00Z", CommentHTML: "PROD-001: PROD-001-B"}}
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		if strings.Contains(joined, "--method\x00GET") && strings.Contains(joined, "/comments/") {
			encoded, _ := json.Marshal(map[string]any{"results": comments})
			return shell.Result{Stdout: string(encoded)}, nil
		}
		if strings.Contains(joined, "--method\x00POST") && strings.Contains(joined, "/comments/") {
			created := planedoc.WorkItemComment{ID: "log", Actor: "looper", CreatedAt: nowISO}
			encoded, _ := json.Marshal(created)
			return shell.Result{Stdout: string(encoded)}, nil
		}
		return shell.Result{}, nil
	}})
	cfg := config.Config{Scheduler: config.SchedulerConfig{RetryMaxAttempts: 3}, Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), ProductOwner: &config.ProductOwnerConfig{PlaneID: "product"}, DesignOwner: &config.FeishuActorConfig{PlaneID: "designer"}}}}
	runtime := &Runtime{config: cfg, now: func() time.Time { return now }, services: Services{Repositories: repos}, planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true }}

	runtime.reconcileAwaitingRoleDecisions(ctx)
	gotLoop, _ := repos.Loops.GetByID(ctx, loopID)
	active, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	gotRun, _ := repos.Runs.GetByID(ctx, "run-late-valid")
	var persisted decisionCheckpointView
	_ = json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted)
	if gotLoop.Status != "paused" || active != nil || persisted.Decisions.Stage != "product_resolved" || persisted.Decisions.Answers["PROD-001"].Value != "PROD-001-B" || persisted.Decisions.Requests[decisions.RoleDesign].CommentID != "" {
		t.Fatalf("late valid replacement did not park/reset: loop=%#v active=%#v state=%#v", gotLoop, active, persisted.Decisions)
	}
	runtime.reconcileAwaitingRoleDecisions(ctx)
	active, _ = repos.Queue.FindActiveByLoopID(ctx, loopID)
	if active == nil {
		t.Fatal("stable replacement snapshot did not requeue after a fresh poll")
	}
}

func TestFormalProductSpecAuthorityMustBeNativeNonEmptyStableAndStillLinked(t *testing.T) {
	repos := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID, loopID, repo, target := "project-formal", "loop-formal", "acme/looper", "issue:acme/looper:1602"
	formalQuestion := decisions.Question{ID: "PROD-000", Role: decisions.RoleProduct, Blocking: true, Question: "请提供正式产品 Spec", Context: "跨页面流程"}
	state := &decisions.State{
		Brief:              decisions.Brief{Version: 1, Revision: 4, Summary: "跨页面流程", FormalProductSpec: decisions.FormalProductSpec{Required: true, Reason: "跨页面"}},
		Stage:              "product_resolved",
		ProductSpec:        "旧产品 Spec",
		Requests:           map[decisions.Role]decisions.RequestReceipt{decisions.RoleProduct: {Role: decisions.RoleProduct, Revision: 4, CommentID: "ask", CreatedAt: "2026-07-17T13:00:00Z"}},
		RequestedQuestions: map[decisions.Role][]decisions.Question{decisions.RoleProduct: {formalQuestion}},
	}
	view := decisionCheckpointView{PipelineVersion: 2, Phase: state.Stage, Decisions: state, Issue: &struct {
		URL string `json:"url"`
	}{URL: "https://plane.example/workspace/projects/project-formal/issues/wi-1602"}}
	checkpointBytes, _ := json.Marshal(view)
	checkpoint := string(checkpointBytes)
	metadata := `{"plannerPipelineVersion":2,"decisionPhase":"product_resolved"}`
	if err := repos.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "P", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Loops.Upsert(ctx, storage.LoopRecord{ID: loopID, Seq: 6, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &target, Repo: &repo, Status: "queued", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	if err := repos.Runs.Upsert(ctx, storage.RunRecord{ID: "run-formal", LoopID: loopID, Status: "paused", CheckpointJSON: &checkpoint, StartedAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	projectIDRef, loopIDRef := projectID, loopID
	if err := repos.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue-formal", ProjectID: &projectIDRef, LoopID: &loopIDRef, Type: "planner", TargetType: "issue", TargetID: target, Repo: &repo, DedupeKey: "planner:" + projectID + ":" + loopID, Priority: storage.QueuePriorityPlanner, Status: "queued", AvailableAt: nowISO, MaxAttempts: 3, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	linkMode := "native"
	pageContent := "新产品 Spec"
	comments := []planedoc.WorkItemComment{}
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		joined := strings.Join(options.Args, "\x00")
		switch {
		case strings.Contains(joined, "api\x00link\x00list"):
			switch linkMode {
			case "missing":
				return shell.Result{Stdout: `{"results":[]}`}, nil
			case "external":
				return shell.Result{Stdout: `{"results":[{"title":"looper:product-spec","url":"https://feishu.example/doc/1"}]}`}, nil
			default:
				return shell.Result{Stdout: `{"results":[{"title":"looper:product-spec","url":"https://plane.example/workspace/projects/project-formal/pages/product-page"}]}`}, nil
			}
		case strings.Contains(joined, "api\x00page\x00get") && strings.Contains(joined, "--content"):
			return shell.Result{Stdout: pageContent}, nil
		case strings.Contains(joined, "--method\x00GET") && strings.Contains(joined, "/comments/"):
			encoded, _ := json.Marshal(map[string]any{"results": comments})
			return shell.Result{Stdout: string(encoded)}, nil
		case strings.Contains(joined, "--method\x00POST") && strings.Contains(joined, "/comments/"):
			commentHTML := ""
			for i, arg := range options.Args {
				if arg == "--data" && i+1 < len(options.Args) {
					var payload map[string]string
					_ = json.Unmarshal([]byte(options.Args[i+1]), &payload)
					commentHTML = payload["comment_html"]
				}
			}
			created := planedoc.WorkItemComment{ID: "log-" + string(rune('a'+len(comments))), Actor: "looper", CreatedAt: nowISO, CommentHTML: commentHTML}
			comments = append(comments, created)
			encoded, _ := json.Marshal(created)
			return shell.Result{Stdout: string(encoded)}, nil
		default:
			return shell.Result{}, nil
		}
	}})
	cfg := config.Config{Scheduler: config.SchedulerConfig{RetryMaxAttempts: 3}, Projects: []config.ProjectRefConfig{{ID: projectID, RepoPath: t.TempDir(), ProductOwner: &config.ProductOwnerConfig{PlaneID: "product"}}}}
	runtime := &Runtime{config: cfg, now: func() time.Time { return now }, services: Services{Repositories: repos}, planeDocFactory: func(*config.Config, string) (*planedoc.Gateway, string, bool) { return gateway, projectID, true }}

	// Changed content invalidates the old queued snapshot and requires a stable re-read.
	runtime.reconcileAwaitingRoleDecisions(ctx)
	gotLoop, _ := repos.Loops.GetByID(ctx, loopID)
	active, _ := repos.Queue.FindActiveByLoopID(ctx, loopID)
	gotRun, _ := repos.Runs.GetByID(ctx, "run-formal")
	var persisted decisionCheckpointView
	_ = json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted)
	if gotLoop.Status != "paused" || active != nil || persisted.Decisions.ProductSpec != pageContent || persisted.Decisions.Stage != "product_resolved" {
		t.Fatalf("changed formal Spec was not parked: loop=%#v active=%#v state=%#v", gotLoop, active, persisted.Decisions)
	}
	runtime.reconcileAwaitingRoleDecisions(ctx)
	active, _ = repos.Queue.FindActiveByLoopID(ctx, loopID)
	if active == nil {
		t.Fatal("stable non-empty native formal Spec did not requeue")
	}

	// Removing the link after resolution reopens the formal gate and cancels the queue.
	linkMode = "missing"
	runtime.reconcileAwaitingRoleDecisions(ctx)
	gotLoop, _ = repos.Loops.GetByID(ctx, loopID)
	active, _ = repos.Queue.FindActiveByLoopID(ctx, loopID)
	gotRun, _ = repos.Runs.GetByID(ctx, "run-formal")
	persisted = decisionCheckpointView{}
	_ = json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted)
	if gotLoop.Status != "paused" || active != nil || persisted.Decisions.Stage != "awaiting_product_spec" || persisted.Decisions.ProductSpec != "" {
		t.Fatalf("missing formal Spec did not reopen: loop=%#v active=%#v state=%#v", gotLoop, active, persisted.Decisions)
	}

	// An external link or a blank native page remains the same formal barrier.
	for _, mode := range []string{"external", "blank"} {
		if mode == "external" {
			linkMode = "external"
			pageContent = "ignored"
		} else {
			linkMode = "native"
			pageContent = "   "
		}
		runtime.reconcileAwaitingRoleDecisions(ctx)
		gotRun, _ = repos.Runs.GetByID(ctx, "run-formal")
		persisted = decisionCheckpointView{}
		_ = json.Unmarshal([]byte(*gotRun.CheckpointJSON), &persisted)
		if persisted.Decisions.Stage != "awaiting_product_spec" || persisted.Decisions.ProductSpec != "" {
			t.Fatalf("%s formal Spec incorrectly satisfied gate: %#v", mode, persisted.Decisions)
		}
	}
}
