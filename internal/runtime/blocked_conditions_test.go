package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/disk"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/infra/shell"
	"github.com/nexu-io/looper/internal/loops"
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
	"github.com/nexu-io/looper/internal/storage"
)

func TestAssociateProductSpecReplyLinksFirstPlaneReplyAfterAsk(t *testing.T) {
	responses := []string{
		`{"results":[{"id":"ask","comment_html":"<p>ask</p>","actor":"looper-owner","created_at":"2026-07-15T12:00:00.000Z"},{"id":"reply","comment_html":"<p><a href=\"https://docs.example/product-spec\">方案</a></p>","actor":"product-owner","created_at":"2026-07-15T12:01:00.000Z"}]}`,
		`{"results":[]}`,
		`{"id":"link-1"}`,
	}
	var calls [][]string
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, options shell.Options) (shell.Result, error) {
		calls = append(calls, options.Args)
		stdout := responses[len(calls)-1]
		return shell.Result{Stdout: stdout}, nil
	}})
	targetID := "issue:nexu-io/open-design:582"
	associated, confirmation, err := associateProductSpecReply(
		context.Background(),
		gateway,
		"plane-project",
		"work-item",
		storage.LoopRecord{TargetID: &targetID},
		loopcondition.Record{Since: "2026-07-15T12:00:30.000Z", Fingerprint: "ask"},
		"product-owner",
	)
	if err != nil || !associated {
		t.Fatalf("associateProductSpecReply() = %v, %v", associated, err)
	}
	if len(calls) != 3 || !strings.Contains(strings.Join(calls[2], " "), "looper:product-spec") {
		t.Fatalf("calls = %v, want comment list then product-spec link lookup/create", calls)
	}
	if confirmation.URL != "https://docs.example/product-spec" || confirmation.PlaneActorID != "product-owner" {
		t.Fatalf("confirmation = %+v", confirmation)
	}
}

func TestAssociateProductSpecReplyIgnoresNonProductActor(t *testing.T) {
	gateway := planedoc.New(planedoc.Options{Workspace: "workspace", Run: func(_ context.Context, _ shell.Options) (shell.Result, error) {
		return shell.Result{Stdout: `{"results":[{"id":"reply","comment_html":"<p>my draft</p>","actor":"looper-owner","created_at":"2026-07-15T12:01:00.000Z"}]}`}, nil
	}})
	associated, _, err := associateProductSpecReply(context.Background(), gateway, "project", "item", storage.LoopRecord{}, loopcondition.Record{Since: "2026-07-15T12:00:00.000Z"}, "product-owner")
	if err != nil || associated {
		t.Fatalf("associateProductSpecReply() = %v, %v; want non-product reply ignored", associated, err)
	}
}

func TestEffectiveBlockedConditionMigratesLegacyMarkers(t *testing.T) {
	productMetadata := `{"awaitingProductSpec":true}`
	record, inferred := effectiveBlockedCondition(storage.LoopRecord{Type: "planner", Status: "paused", MetadataJSON: &productMetadata})
	if !inferred || record.Kind != loopcondition.ProductSpec {
		t.Fatalf("effectiveBlockedCondition(product) = %#v, %v", record, inferred)
	}
	humanMetadata := `{"hitl":{"question":"pick","status":"awaiting"}}`
	record, inferred = effectiveBlockedCondition(storage.LoopRecord{Type: "worker", Status: "awaiting_human", MetadataJSON: &humanMetadata})
	if !inferred || record.Kind != loopcondition.HumanAnswered {
		t.Fatalf("effectiveBlockedCondition(human) = %#v, %v", record, inferred)
	}
}

func TestRecordPlaneHITLAnswerPersistsSourceAnswer(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	nowISO := "2026-07-15T12:00:00.000Z"
	ask := loops.HITLAsk{Question: "A or B?", Status: "awaiting", AskedAt: nowISO, Transport: "plane", ActionURL: "https://plane.test/pages/pg-1#comment-ask"}
	metadata, err := loops.WriteHITLAsk(nil, ask)
	if err != nil {
		t.Fatal(err)
	}
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_1", Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: "loop_plane_answer", Seq: 9, ProjectID: "project_1", Type: "planner", TargetType: "issue", Status: "awaiting_human", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := recordPlaneHITLAnswer(ctx, repositories, loop.ID, "Choose B", "2026-07-15T12:01:00.000Z"); err != nil {
		t.Fatal(err)
	}
	got, _ := repositories.Loops.GetByID(ctx, loop.ID)
	gotAsk, ok := loops.ReadHITLAsk(got.MetadataJSON)
	if !ok || gotAsk.Status != "answered" || gotAsk.Answer != "Choose B" || gotAsk.AnsweredAt != "2026-07-15T12:01:00.000Z" {
		t.Fatalf("persisted ask = %#v, present=%v", gotAsk, ok)
	}
}

func TestCollectWorkItemDecisionAnswerMergesProductReplyBurst(t *testing.T) {
	t.Parallel()

	askedAt := time.Date(2026, time.July, 16, 3, 0, 0, 0, time.UTC)
	comments := []planedoc.WorkItemComment{
		{ID: "third", Actor: "product-owner", CommentStripped: "静默更新在按钮下方", CreatedAt: "2026-07-16T03:03:00.000Z"},
		{ID: "looper", CommentStripped: "ask · Powered by Looper", CreatedAt: "2026-07-16T03:01:00.000Z"},
		{ID: "before", CommentStripped: "旧评论", CreatedAt: "2026-07-16T02:59:00.000Z"},
		{ID: "other", Actor: "looper-owner", CommentStripped: "我觉得可以", CreatedAt: "2026-07-16T03:02:30.000Z"},
		{ID: "first", Actor: "product-owner", CommentHTML: "<p>later</p>", CreatedAt: "2026-07-16T03:02:00.000Z"},
		{ID: "second", Actor: "product-owner", CommentStripped: "install", CreatedAt: "2026-07-16T03:02:10.000Z"},
	}

	answer, answeredAt, ok := collectWorkItemDecisionAnswer(comments, askedAt, "product-owner")
	if !ok || answer != "<p>later</p>\n\ninstall\n\n静默更新在按钮下方" {
		t.Fatalf("answer = %q, ok=%v, want all product replies in chronological order", answer, ok)
	}
	if answeredAt != "2026-07-16T03:03:00.000Z" {
		t.Fatalf("answeredAt = %q, want latest product reply time", answeredAt)
	}
}

func TestBlockedConditionRegistryContainsEveryNamedCondition(t *testing.T) {
	runtime := &Runtime{}
	registry := runtime.blockedConditionRegistry(&config.Config{}, nil, nil)
	for _, kind := range []loopcondition.Kind{
		loopcondition.ProductSpec,
		loopcondition.DiskRecovered,
		loopcondition.CISettled,
		loopcondition.ReviewUpdated,
		loopcondition.HumanAnswered,
		loopcondition.InfraRecovered,
	} {
		if registry[kind] == nil {
			t.Fatalf("registry[%q] is nil", kind)
		}
	}
}

func TestLatestRunIssueURLFallsBackToPlannerCheckpoint(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	checkpoint := `{"issue":{"url":"https://plane.example/workspaces/w/projects/p/issues/wi-1"}}`
	now := "2026-07-15T12:00:00.000Z"
	ctx := context.Background()
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: "project_product_spec", Name: "Project", RepoPath: t.TempDir(), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Loops.Upsert(ctx, storage.LoopRecord{ID: "loop_product_spec", Seq: 11, ProjectID: "project_product_spec", Type: "planner", TargetType: "issue", Status: "paused", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := repositories.Runs.Upsert(ctx, storage.RunRecord{ID: "run_product_spec", LoopID: "loop_product_spec", Status: "failed", CheckpointJSON: &checkpoint, StartedAt: now, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	url, err := latestRunIssueURL(ctx, repositories, "loop_product_spec")
	if err != nil || url != "https://plane.example/workspaces/w/projects/p/issues/wi-1" {
		t.Fatalf("latestRunIssueURL() = %q, %v", url, err)
	}
}

func TestResumeBlockedLoopClearsConditionAndRequeues(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_1"
	loopID := "loop_blocked"
	targetID := "project:project_1"
	metadata, err := loopcondition.Set(nil, loopcondition.Record{Kind: loopcondition.DiskRecovered, Since: nowISO})
	if err != nil {
		t.Fatalf("condition.Set() error = %v", err)
	}
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 1, ProjectID: projectID, Type: "worker", TargetType: "project", TargetID: &targetID, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatalf("Loops.Upsert() error = %v", err)
	}
	manualKind := "manual_intervention"
	if err := repositories.Queue.Upsert(ctx, storage.QueueItemRecord{ID: "queue_blocked", ProjectID: &projectID, LoopID: &loopID, Type: "worker", TargetType: "project", TargetID: targetID, DedupeKey: "worker:loop_blocked", Priority: 1, Status: "manual_intervention", AvailableAt: nowISO, Attempts: 1, MaxAttempts: -1, LastErrorKind: &manualKind, CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatalf("Queue.Upsert() error = %v", err)
	}
	if err := resumeBlockedLoop(ctx, repositories, loop, loopcondition.Record{Kind: loopcondition.DiskRecovered}, func() time.Time { return now }); err != nil {
		t.Fatalf("resumeBlockedLoop() error = %v", err)
	}
	gotLoop, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil || gotLoop == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", gotLoop, err)
	}
	if gotLoop.Status != "queued" || gotLoop.NextRunAt == nil || *gotLoop.NextRunAt != "2026-07-15T12:00:01.000Z" {
		t.Fatalf("loop after resume = %#v", gotLoop)
	}
	if _, ok := loopcondition.Read(gotLoop.MetadataJSON); ok {
		t.Fatal("blocked condition was not cleared")
	}
	active, err := repositories.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || active == nil || active.Status != "queued" {
		t.Fatalf("active queue after resume = %#v, %v", active, err)
	}
}

func TestResumeProductSpecLoopClearsLegacyWaitMarker(t *testing.T) {
	repositories := newEnqueueTestRepos(t)
	ctx := context.Background()
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	nowISO := formatJavaScriptISOString(now)
	projectID := "project_product_spec_resume"
	loopID := "loop_product_spec_resume"
	repo := "owner/repo"
	targetID := "issue:owner/repo:42"
	metadata := `{"awaitingProductSpec":true,"blockedCondition":{"kind":"product_spec","since":"2026-07-15T11:00:00.000Z"}}`
	if err := repositories.Projects.Upsert(ctx, storage.ProjectRecord{ID: projectID, Name: "Project", RepoPath: t.TempDir(), CreatedAt: nowISO, UpdatedAt: nowISO}); err != nil {
		t.Fatal(err)
	}
	loop := storage.LoopRecord{ID: loopID, Seq: 2, ProjectID: projectID, Type: "planner", TargetType: "issue", TargetID: &targetID, Repo: &repo, Status: "paused", MetadataJSON: &metadata, CreatedAt: nowISO, UpdatedAt: nowISO}
	if err := repositories.Loops.Upsert(ctx, loop); err != nil {
		t.Fatal(err)
	}
	if err := resumeBlockedLoop(ctx, repositories, loop, loopcondition.Record{Kind: loopcondition.ProductSpec}, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	got, err := repositories.Loops.GetByID(ctx, loopID)
	if err != nil || got == nil {
		t.Fatalf("Loops.GetByID() = %#v, %v", got, err)
	}
	if metadataBool(got.MetadataJSON, "awaitingProductSpec") {
		t.Fatalf("awaitingProductSpec remained true after resume: %s", derefString(got.MetadataJSON))
	}
	if _, inferred := effectiveBlockedCondition(*got); inferred {
		t.Fatalf("resumed loop still infers product-spec blocking: %s", derefString(got.MetadataJSON))
	}
}

func TestDiskConditionClearedUsesHighWatermark(t *testing.T) {
	original := diskUsageStat
	t.Cleanup(func() { diskUsageStat = original })
	cfg := &config.Config{}
	cfg.Daemon.DiskBackpressure.Enabled = true
	cfg.Daemon.DiskBackpressure.Path = "/worktrees"
	cfg.Daemon.DiskBackpressure.HighWatermarkPercent = 85
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{UsedPercent: 84.9}, nil }
	if ready, err := diskConditionCleared(cfg); err != nil || !ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want ready", ready, err)
	}
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{UsedPercent: 85}, nil }
	if ready, err := diskConditionCleared(cfg); err != nil || ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want blocked", ready, err)
	}
	diskUsageStat = func(string) (disk.Usage, error) { return disk.Usage{}, errors.New("stat failed") }
	if ready, err := diskConditionCleared(cfg); err == nil || ready {
		t.Fatalf("diskConditionCleared() = %v, %v; want observable check failure", ready, err)
	}
}
