package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

func TestDetectGitHubHITLAnswer(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 100, Author: "lefarcen", Body: "<!-- looper:hitl:ask v=1 --> which one?"}, // the ask (bot marker), == askCommentID
		{ID: 101, Author: "lefarcen", Body: "<!-- looper:stamp --> still working"},     // bot marker, ignored even if same login
		{ID: 105, Author: "lefarcen", Body: "用 A,改 resize handle"},                     // human reply, no marker -> first answer
		{ID: 110, Author: "someoneelse", Body: "later comment"},
	}
	// First non-looper comment after the ask wins — even though the bot and human
	// share the "lefarcen" account, the marker distinguishes them.
	if got := detectGitHubHITLAnswer(comments, 100, nil); got != "用 A,改 resize handle" {
		t.Fatalf("answer = %q, want the first human reply", got)
	}
	// Only the ask + a marked bot note -> no answer yet.
	if got := detectGitHubHITLAnswer(comments[:2], 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (no human reply yet)", got)
	}
	// Allowlist excludes lefarcen -> the next allowed author answers.
	if got := detectGitHubHITLAnswer(comments, 100, []string{"someoneelse"}); got != "later comment" {
		t.Fatalf("answer = %q, want the allowlisted author's comment", got)
	}
	// A looper-marked comment after the ask is never an answer.
	marked := []githubAnswerComment{{ID: 200, Author: "lefarcen", Body: "<!-- looper:decision-log --> recorded"}}
	if got := detectGitHubHITLAnswer(marked, 100, nil); got != "" {
		t.Fatalf("answer = %q, want empty (looper's own comment)", got)
	}
}

func TestPollGitHubHITLAnswersOnce(t *testing.T) {
	commentsByPR := map[int64][]githubAnswerComment{
		42: {{ID: 500, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}, {ID: 501, Author: "lefarcen", Body: "go with A"}},
		43: {{ID: 600, Author: "lefarcen", Body: "<!-- looper:hitl:ask --> ask"}}, // no human reply yet
	}
	var deliveredTo []string
	var cleared []int64
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, pr int64, _ string) ([]githubAnswerComment, error) {
			return commentsByPR[pr], nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			deliveredTo = append(deliveredTo, loopID+"="+answer)
			return nil
		},
		clearAwaiting: func(_ contextType, _ string, pr int64, _ string) { cleared = append(cleared, pr) },
		projectCWD:    func(string) string { return "/tmp/repo" },
	}
	loops := []githubHITLAwaitingLoop{
		{ID: "loop-a", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 500},
		{ID: "loop-b", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 43, AskCommentID: 600},
		{ID: "loop-c", Repo: "acme/x", Transport: "feishu", PRNumber: 44}, // non-github, skipped
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), loops, deps)
	if n != 1 {
		t.Fatalf("delivered = %d, want 1", n)
	}
	if len(deliveredTo) != 1 || deliveredTo[0] != "loop-a=go with A" {
		t.Fatalf("deliveredTo = %v, want [loop-a=go with A]", deliveredTo)
	}
	if len(cleared) != 1 || cleared[0] != 42 {
		t.Fatalf("cleared = %v, want [42]", cleared)
	}
}

func TestGitHubHITLPollDeliversAndContinuesReviewFixBudget(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	nowISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	coordinator, err := storage.OpenSQLiteCoordinator(context.Background(), filepath.Join(root, "looper.sqlite"), storage.SQLiteCoordinatorOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(context.Background(), storage.RunPendingOptions{}); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	repos := storage.NewRepositories(coordinator.DB())
	const projectID = "project_budget_github"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":8}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_reviewer", Seq: 11, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_fixer", Seq: 12, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	seedPollBudgetQueue(t, repos, nowISO, "queue_budget_reviewer", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedPollBudgetQueue(t, repos, nowISO, "queue_budget_fixer", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	ask, ok := loops.ReadHITLAsk(parked.MetadataJSON)
	if !ok || !loops.IsReviewFixBudgetAsk(ask) || ask.PRNumber != pr || ask.Transport != "" || ask.AskCommentID != 0 {
		t.Fatalf("parked ask = %#v, want budget ask with PR and no GitHub transport yet", ask)
	}

	var posted []string
	var labeled []string
	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
		createComment: func(_ contextType, gotRepo string, gotPR int64, body, _ string) (int64, error) {
			posted = append(posted, body)
			if gotRepo != repo || gotPR != pr {
				t.Fatalf("createComment target = %s#%d, want %s#%d", gotRepo, gotPR, repo, pr)
			}
			if !strings.Contains(body, "<!-- looper:hitl:ask v=1") || !strings.Contains(body, ask.Question) || !strings.Contains(body, "Continue") {
				t.Fatalf("createComment body missing ask marker/question/options: %s", body)
			}
			return 9001, nil
		},
		addLabel: func(_ contextType, _ string, gotPR int64, label, _ string) {
			labeled = append(labeled, label)
			if gotPR != pr {
				t.Fatalf("addLabel pr = %d, want %d", gotPR, pr)
			}
		},
		projectCWD:    func(string) string { return root },
		mentionLogins: []string{"operator"},
		awaitingLabel: "looper:awaiting-human",
		nowISO:        nowISO,
	})
	if delivered != 1 {
		t.Fatalf("deliverUndeliveredGitHubBudgetAsks() = %d, want 1", delivered)
	}
	if len(posted) != 1 || !strings.Contains(posted[0], "@operator") {
		t.Fatalf("posted = %#v, want one mention of @operator", posted)
	}
	if len(labeled) != 1 || labeled[0] != "looper:awaiting-human" {
		t.Fatalf("labeled = %#v, want awaiting-human", labeled)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
	}
	ask, ok = loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Transport != "github" || ask.PRNumber != pr || ask.AskCommentID != 9001 {
		t.Fatalf("delivered ask = %#v, want github transport + comment 9001", ask)
	}

	var answers []string
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: reviewer.ID, ProjectID: projectID, Repo: repo, Transport: "github",
		AskStatus: "awaiting", PRNumber: pr, AskCommentID: 9001,
	}}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 9001, Author: "looper", Body: posted[0]},
				{ID: 9002, Author: "operator", Body: "Continue"},
			}, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			answers = append(answers, loopID+"="+answer)
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(_ contextType, _ string, _ int64, _ string) {},
		projectCWD:    func(string) string { return root },
	})
	if n != 1 || len(answers) != 1 || answers[0] != reviewer.ID+"=Continue" {
		t.Fatalf("poll delivered = %d answers=%v, want Continue on reviewer", n, answers)
	}
	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" || loops.ReviewerPublishCount(fresh.MetadataJSON) != 0 {
		t.Fatalf("reviewer after continue = (%#v, %v), want queued with reset count", fresh, err)
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "queued" || loops.IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		t.Fatalf("fixer after continue = (%#v, %v), want queued and unpaused", sibling, err)
	}
	for _, queueID := range []string{"queue_budget_reviewer", "queue_budget_fixer"} {
		queue, err := repos.Queue.GetByID(context.Background(), queueID)
		if err != nil || queue == nil || queue.Status != "queued" {
			t.Fatalf("%s after continue = (%#v, %v), want queued", queueID, queue, err)
		}
	}
}

func seedPollBudgetQueue(t *testing.T, repos *storage.Repositories, nowISO, id, loopID, queueType string, priority int64) {
	t.Helper()
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: id, ProjectID: stringPtr("project_budget_github"), LoopID: &loopID, Type: queueType,
		TargetType: "pull_request", TargetID: "pr:acme/looper:42", Status: "queued",
		Priority: priority, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		DedupeKey: queueType + ":" + id,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}
