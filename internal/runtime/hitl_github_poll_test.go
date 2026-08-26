package runtime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/agent"
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

func TestDetectGitHubHITLAnswerSkipsUnrelatedBudgetComments(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 9001, Author: "looper", Body: "<!-- looper:hitl:ask --> Continue or Stop?"},
		{ID: 9002, Author: "operator", Body: "looking into this first"},
		{ID: 9003, Author: "operator", Body: "Continue investigating; do not resume yet"},
		{ID: 9004, Author: "operator", Body: "Continue"},
	}
	accept := func(body string) bool {
		return loops.IsReviewFixBudgetContinue(body) || loops.IsReviewFixBudgetStop(body)
	}
	if got := detectGitHubHITLAnswerMatching(comments, 9001, nil, accept); got != "Continue" {
		t.Fatalf("budget answer = %q, want exact Continue after skipping prefix-only discussion", got)
	}
	if got := detectGitHubHITLAnswer(comments, 9001, nil); got != "looking into this first" {
		t.Fatalf("generic answer = %q, want earliest human comment", got)
	}
}

func TestPollGitHubHITLAnswersOnceScopeAskSkipsUnrelatedComments(t *testing.T) {
	// Scope asks must use Continue/Stop-only filtering (same as budget asks).
	comments := []githubAnswerComment{
		{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md before unpause"},
		{ID: 8002, Author: "operator", Body: "unrelated discussion about the PR"},
		{ID: 8003, Author: "operator", Body: "Continue"},
	}
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return comments, nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return nil
		},
	}
	// BudgetAsk=true is set for scope asks at the poll assembly site.
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: "loop-scope", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 8001, BudgetAsk: true},
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != "loop-scope=Continue" {
		t.Fatalf("delivered = %#v n=%d, want only Continue for scope ask", delivered, n)
	}
	// Without BudgetAsk filtering, the unrelated comment would win.
	n = pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: "loop-scope-open", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 8001, BudgetAsk: false},
	}, deps)
	if n != 1 || len(delivered) != 2 || delivered[1] != "loop-scope-open=unrelated discussion about the PR" {
		t.Fatalf("open ask delivered = %#v n=%d, want earliest unrelated comment", delivered, n)
	}
}

func TestPollGitHubHITLAnswersOnceAcceptsContinueAndStopForOrdinaryAsk(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"Continue", "Stop"} {
		t.Run(answer, func(t *testing.T) {
			comments := []githubAnswerComment{
				{ID: 9001, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach should we take?"},
				{ID: 9002, Author: "operator", Body: answer},
			}
			var delivered []string
			deps := githubHITLPollDeps{
				listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
					return comments, nil
				},
				deliverAnswer: func(_ contextType, loopID, got string) error {
					delivered = append(delivered, loopID+"="+got)
					return nil
				},
			}
			n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
				{ID: "loop-ordinary", Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 9001, BudgetAsk: false},
			}, deps)
			if n != 1 || len(delivered) != 1 || delivered[0] != "loop-ordinary="+answer {
				t.Fatalf("delivered = %#v n=%d, want %s for ordinary GitHub ask", delivered, n, answer)
			}
		})
	}
}

func TestGitHubHITLDecisionOnlyAskIncludesScopeOverlay(t *testing.T) {
	t.Parallel()
	agentAsk := loops.HITLAsk{
		Status: "awaiting", Transport: "github", PRNumber: 42, AskCommentID: 8001,
		Question: "unrelated agent question on the sibling",
	}
	meta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	meta, err = loops.WriteReviewScopeHumanState(&meta, loops.ReviewScopeHumanState{
		HeldBy: "reviewer", PauseReason: loops.ReviewScopeHumanSiblingPauseReason,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	})
	if err != nil {
		t.Fatalf("WriteReviewScopeHumanState: %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_fix", Status: "awaiting_human", MetadataJSON: &meta}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok || loops.IsReviewScopeHumanAsk(ask) || loops.IsReviewFixBudgetAsk(ask) {
		t.Fatalf("ask = (%#v, %v), want preserved agent ask", ask, ok)
	}
	if !loops.IsReviewScopeHumanHold(loop) {
		t.Fatal("want scope overlay hold")
	}
	if !githubHITLDecisionOnlyAsk(loop, ask) {
		t.Fatal("scope overlay must force Continue/Stop-only GitHub filtering")
	}
}

func TestPollGitHubHITLAnswersOnceScopeOverlaySkipsUnrelatedComments(t *testing.T) {
	comments := []githubAnswerComment{
		{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
		{ID: 8002, Author: "operator", Body: "unrelated discussion about the PR"},
		{ID: 8003, Author: "operator", Body: "Continue"},
	}
	agentAsk := loops.HITLAsk{
		Status: "awaiting", Transport: "github", PRNumber: 42, AskCommentID: 8001,
		Question: "unrelated agent question on the sibling",
	}
	meta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk: %v", err)
	}
	meta, err = loops.WriteReviewScopeHumanState(&meta, loops.ReviewScopeHumanState{
		HeldBy: "reviewer", PauseReason: loops.ReviewScopeHumanSiblingPauseReason,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	})
	if err != nil {
		t.Fatalf("WriteReviewScopeHumanState: %v", err)
	}
	loop := storage.LoopRecord{ID: "loop_overlay_poll", Status: "awaiting_human", MetadataJSON: &meta}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		t.Fatal("want preserved agent ask")
	}
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return comments, nil
		},
		deliverAnswer: func(_ contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return nil
		},
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		{ID: loop.ID, Repo: "acme/x", Transport: "github", AskStatus: "awaiting", PRNumber: 42, AskCommentID: 8001, BudgetAsk: githubHITLDecisionOnlyAsk(loop, ask)},
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != loop.ID+"=Continue" {
		t.Fatalf("delivered = %#v n=%d, want Continue after skipping overlay chatter", delivered, n)
	}
}

func TestPollGitHubHITLAnswersOnceConsumesOneScopeDecisionPerPair(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		siblingFirst bool
	}{
		{name: "primary_first"},
		{name: "sibling_first", siblingFirst: true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
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
			projectID := "project_scope_pair_" + tc.name
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "acme/looper"
			pr := int64(42)
			target := "pr:acme/looper:42"
			reviewer := storage.LoopRecord{
				ID: "loop_scope_pair_reviewer_" + tc.name, Seq: 31, ProjectID: projectID, Type: "reviewer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			fixer := storage.LoopRecord{
				ID: "loop_scope_pair_fixer_" + tc.name, Seq: 32, ProjectID: projectID, Type: "fixer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			agentAsk := loops.HITLAsk{
				Kind: "agent_question", Question: "Which approach should Fixer take?",
				Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
				Transport: "github", PRNumber: pr, AskCommentID: 8002,
			}
			fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
			if err != nil {
				t.Fatalf("WriteHITLAsk(fixer): %v", err)
			}
			fixer.MetadataJSON = &fixerMeta
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
				t.Fatalf("Loops.Upsert(fixer) error = %v", err)
			}

			if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
				Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
				NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md vs PR non-goals before continue.",
			}); err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			reviewer = stampGitHubHITLAsk(t, repos, reviewer.ID, 8001)
			fixer = stampGitHubHITLAsk(t, repos, fixer.ID, 8002)
			reviewerAsk, ok := loops.ReadHITLAsk(reviewer.MetadataJSON)
			if !ok || !loops.IsReviewScopeHumanAsk(reviewerAsk) {
				t.Fatalf("reviewer ask = (%#v, %v), want scope ask", reviewerAsk, ok)
			}
			fixerAsk, ok := loops.ReadHITLAsk(fixer.MetadataJSON)
			if !ok || loops.IsReviewScopeHumanAsk(fixerAsk) || fixerAsk.Question != agentAsk.Question {
				t.Fatalf("fixer ask = (%#v, %v), want preserved agent ask", fixerAsk, ok)
			}
			if !githubHITLDecisionOnlyAsk(reviewer, reviewerAsk) || !githubHITLDecisionOnlyAsk(fixer, fixerAsk) {
				t.Fatal("both pair members must snapshot as decision-only")
			}

			primary := githubHITLAwaitingFrom(reviewer, reviewerAsk)
			overlay := githubHITLAwaitingFrom(fixer, fixerAsk)
			awaiting := []githubHITLAwaitingLoop{primary, overlay}
			if tc.siblingFirst {
				awaiting = []githubHITLAwaitingLoop{overlay, primary}
			}
			var cleared []int64
			n := pollGitHubHITLAnswersOnce(context.Background(), awaiting, githubHITLPollDeps{
				listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
					return []githubAnswerComment{
						{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
						{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
						{ID: 8003, Author: "operator", Body: "Continue"},
					}, nil
				},
				deliverAnswer: func(ctx contextType, loopID, answer string) error {
					return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
				},
				clearAwaiting: func(_ contextType, _ string, pr int64, _ string) {
					cleared = append(cleared, pr)
				},
				remainingAwaiting: func(ctx contextType, repo string, pr int64) bool {
					return githubHITLPRHasRemainingAwaiting(ctx, repos, projectID, repo, pr)
				},
			})
			if n != 1 {
				t.Fatalf("delivered = %d, want one pair-level Continue", n)
			}

			freshReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || freshReviewer == nil || freshReviewer.Status != "queued" || loops.IsReviewScopeHumanHold(*freshReviewer) {
				t.Fatalf("reviewer after Continue = (%#v, %v), want queued and released", freshReviewer, err)
			}
			if _, stillAsked := loops.ReadHITLAsk(freshReviewer.MetadataJSON); stillAsked {
				t.Fatal("reviewer still has a HITL ask after pair Continue")
			}
			freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
			if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" || loops.IsReviewScopeHumanHold(*freshFixer) {
				t.Fatalf("fixer after Continue = (%#v, %v), want awaiting preserved agent ask without overlay", freshFixer, err)
			}
			remaining, ok := loops.ReadHITLAsk(freshFixer.MetadataJSON)
			if !ok || remaining.Status != "awaiting" || remaining.Question != agentAsk.Question || remaining.Answer != "" {
				t.Fatalf("fixer ask after Continue = (%#v, %v), want unanswered agent question", remaining, ok)
			}
			if len(cleared) != 0 {
				t.Fatalf("cleared = %v, want empty while sibling agent ask remains", cleared)
			}
		})
	}
}

func TestPollGitHubHITLAnswersOnceDoesNotReuseScopeContinueOnSecondPass(t *testing.T) {
	t.Parallel()
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
	const projectID = "project_scope_second_pass"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_scope_second_pass_reviewer", Seq: 41, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_second_pass_fixer", Seq: 42, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8002,
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reviewer = stampGitHubHITLAsk(t, repos, reviewer.ID, 8001)
	fixer = stampGitHubHITLAsk(t, repos, fixer.ID, 8002)
	reviewerAsk, ok := loops.ReadHITLAsk(reviewer.MetadataJSON)
	if !ok || !loops.IsReviewScopeHumanAsk(reviewerAsk) {
		t.Fatalf("reviewer ask = (%#v, %v), want scope ask", reviewerAsk, ok)
	}
	fixerAsk, ok := loops.ReadHITLAsk(fixer.MetadataJSON)
	if !ok || loops.IsReviewScopeHumanAsk(fixerAsk) {
		t.Fatalf("fixer ask = (%#v, %v), want preserved agent ask", fixerAsk, ok)
	}

	comments := []githubAnswerComment{
		{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
		{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
		{ID: 8003, Author: "operator", Body: "use approach A"},
		{ID: 8004, Author: "operator", Body: "Continue"},
	}
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return comments, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		advanceAskPastComment: func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error {
			return advanceSiblingGitHubHITLAsksPastComment(ctx, repos, projectID, repo, prNumber, exceptLoopID, commentID, comments, nil)
		},
		remainingAwaiting: func(ctx contextType, repo string, pr int64) bool {
			return githubHITLPRHasRemainingAwaiting(ctx, repos, projectID, repo, pr)
		},
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(reviewer, reviewerAsk),
		githubHITLAwaitingFrom(fixer, fixerAsk),
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != reviewer.ID+"=Continue" {
		t.Fatalf("first pass delivered = %#v n=%d, want reviewer Continue", delivered, n)
	}

	freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" || loops.IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer after first pass = (%#v, %v), want awaiting preserved ask", freshFixer, err)
	}
	remaining, ok := loops.ReadHITLAsk(freshFixer.MetadataJSON)
	if !ok || remaining.Status != "awaiting" || remaining.Question != agentAsk.Question || remaining.Answer != "" {
		t.Fatalf("fixer ask after first pass = (%#v, %v), want unanswered agent question", remaining, ok)
	}
	if remaining.AskCommentID != 8002 {
		t.Fatalf("fixer AskCommentID = %d, want original ordinary ask 8002 (not Continue 8004)", remaining.AskCommentID)
	}

	second := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(*freshFixer, remaining),
	}, deps)
	if second != 1 || len(delivered) != 2 || delivered[1] != fixer.ID+"=use approach A" {
		t.Fatalf("second pass delivered = %#v n=%d, want earlier agent answer not Continue", delivered, second)
	}
}

func TestPollGitHubHITLAnswersOnceDoesNotReuseConsumedContinueWithoutEarlierText(t *testing.T) {
	t.Parallel()
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
	const projectID = "project_scope_continue_only"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_scope_continue_only_reviewer", Seq: 51, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_continue_only_fixer", Seq: 52, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8002,
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reviewer = stampGitHubHITLAsk(t, repos, reviewer.ID, 8001)
	fixer = stampGitHubHITLAsk(t, repos, fixer.ID, 8002)
	reviewerAsk, ok := loops.ReadHITLAsk(reviewer.MetadataJSON)
	if !ok || !loops.IsReviewScopeHumanAsk(reviewerAsk) {
		t.Fatalf("reviewer ask = (%#v, %v), want scope ask", reviewerAsk, ok)
	}
	fixerAsk, ok := loops.ReadHITLAsk(fixer.MetadataJSON)
	if !ok || loops.IsReviewScopeHumanAsk(fixerAsk) {
		t.Fatalf("fixer ask = (%#v, %v), want preserved agent ask", fixerAsk, ok)
	}

	comments := []githubAnswerComment{
		{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
		{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
		{ID: 8004, Author: "operator", Body: "Continue"},
	}
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return comments, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		advanceAskPastComment: func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error {
			return advanceSiblingGitHubHITLAsksPastComment(ctx, repos, projectID, repo, prNumber, exceptLoopID, commentID, comments, nil)
		},
		remainingAwaiting: func(ctx contextType, repo string, pr int64) bool {
			return githubHITLPRHasRemainingAwaiting(ctx, repos, projectID, repo, pr)
		},
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(reviewer, reviewerAsk),
		githubHITLAwaitingFrom(fixer, fixerAsk),
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != reviewer.ID+"=Continue" {
		t.Fatalf("first pass delivered = %#v n=%d, want reviewer Continue", delivered, n)
	}

	freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" || loops.IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer after first pass = (%#v, %v), want awaiting preserved ask", freshFixer, err)
	}
	remaining, ok := loops.ReadHITLAsk(freshFixer.MetadataJSON)
	if !ok || remaining.Status != "awaiting" || remaining.Question != agentAsk.Question || remaining.Answer != "" {
		t.Fatalf("fixer ask after first pass = (%#v, %v), want unanswered agent question", remaining, ok)
	}
	if remaining.AskCommentID != 8004 {
		t.Fatalf("fixer AskCommentID = %d, want consumed Continue 8004 so it cannot answer the ordinary ask", remaining.AskCommentID)
	}

	second := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(*freshFixer, remaining),
	}, deps)
	if second != 0 || len(delivered) != 1 {
		t.Fatalf("second pass delivered = %#v n=%d, want no reuse of consumed Continue", delivered, second)
	}
}

func TestPollGitHubHITLAnswersOnceIgnoresUnauthorizedEarlierSiblingText(t *testing.T) {
	t.Parallel()
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
	const projectID = "project_scope_unauthorized_earlier"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_scope_unauth_earlier_reviewer", Seq: 61, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_unauth_earlier_fixer", Seq: 62, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8002,
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reviewer = stampGitHubHITLAsk(t, repos, reviewer.ID, 8001)
	fixer = stampGitHubHITLAsk(t, repos, fixer.ID, 8002)
	reviewerAsk, ok := loops.ReadHITLAsk(reviewer.MetadataJSON)
	if !ok || !loops.IsReviewScopeHumanAsk(reviewerAsk) {
		t.Fatalf("reviewer ask = (%#v, %v), want scope ask", reviewerAsk, ok)
	}
	fixerAsk, ok := loops.ReadHITLAsk(fixer.MetadataJSON)
	if !ok || loops.IsReviewScopeHumanAsk(fixerAsk) {
		t.Fatalf("fixer ask = (%#v, %v), want preserved agent ask", fixerAsk, ok)
	}

	allowed := []string{"operator"}
	comments := []githubAnswerComment{
		{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
		{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
		{ID: 8003, Author: "outsider", Body: "use approach A"},
		{ID: 8004, Author: "operator", Body: "Continue"},
	}
	var delivered []string
	deps := githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return comments, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		advanceAskPastComment: func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error {
			return advanceSiblingGitHubHITLAsksPastComment(ctx, repos, projectID, repo, prNumber, exceptLoopID, commentID, comments, allowed)
		},
		remainingAwaiting: func(ctx contextType, repo string, pr int64) bool {
			return githubHITLPRHasRemainingAwaiting(ctx, repos, projectID, repo, pr)
		},
		answerAuthors: allowed,
	}
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(reviewer, reviewerAsk),
		githubHITLAwaitingFrom(fixer, fixerAsk),
	}, deps)
	if n != 1 || len(delivered) != 1 || delivered[0] != reviewer.ID+"=Continue" {
		t.Fatalf("first pass delivered = %#v n=%d, want reviewer Continue", delivered, n)
	}

	freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" || loops.IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer after first pass = (%#v, %v), want awaiting preserved ask", freshFixer, err)
	}
	remaining, ok := loops.ReadHITLAsk(freshFixer.MetadataJSON)
	if !ok || remaining.Status != "awaiting" || remaining.Question != agentAsk.Question || remaining.Answer != "" {
		t.Fatalf("fixer ask after first pass = (%#v, %v), want unanswered agent question", remaining, ok)
	}
	if remaining.AskCommentID != 8004 {
		t.Fatalf("fixer AskCommentID = %d, want consumed Continue 8004; unauthorized earlier text is not a preserved answer", remaining.AskCommentID)
	}

	second := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(*freshFixer, remaining),
	}, deps)
	if second != 0 || len(delivered) != 1 {
		t.Fatalf("second pass delivered = %#v n=%d, want no reuse of consumed Continue", delivered, second)
	}
}

func TestPollGitHubHITLAnswersOnceLimitsConsumedDecisionToPairLane(t *testing.T) {
	t.Parallel()
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
	const projectID = "project_scope_multi_lane"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	autoReviewer := storage.LoopRecord{
		ID: "loop_multi_lane_auto_rev", Seq: 51, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	autoFixer := storage.LoopRecord{
		ID: "loop_multi_lane_auto_fix", Seq: 52, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	autoAgentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which automatic approach?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8002,
	}
	autoFixerMeta, err := loops.WriteHITLAsk(nil, autoAgentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(auto fixer): %v", err)
	}
	autoFixer.MetadataJSON = &autoFixerMeta
	contMeta := `{"manual":true,"followUpdates":true}`
	contAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which continuous-manual approach?",
		Options: []string{"C", "D"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8003,
	}
	contAskMeta, err := loops.WriteHITLAsk(&contMeta, contAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(continuous fixer): %v", err)
	}
	contFixer := storage.LoopRecord{
		ID: "loop_multi_lane_cont_fix", Seq: 53, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &contAskMeta,
	}
	oneShotMeta := `{"manual":true,"followUpdates":false}`
	oneShotAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which one-shot approach?",
		Options: []string{"E", "F"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8005,
	}
	oneShotAskMeta, err := loops.WriteHITLAsk(&oneShotMeta, oneShotAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(one-shot fixer): %v", err)
	}
	oneShotFixer := storage.LoopRecord{
		ID: "loop_multi_lane_oneshot_fix", Seq: 54, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &oneShotAskMeta,
	}
	for _, loop := range []storage.LoopRecord{autoReviewer, autoFixer, contFixer, oneShotFixer} {
		if err := repos.Loops.Upsert(context.Background(), loop); err != nil {
			t.Fatalf("Loops.Upsert(%s) error = %v", loop.ID, err)
		}
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: autoReviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	autoReviewer = stampGitHubHITLAsk(t, repos, autoReviewer.ID, 8001)
	autoFixer = stampGitHubHITLAsk(t, repos, autoFixer.ID, 8002)
	contFixer = stampGitHubHITLAsk(t, repos, contFixer.ID, 8003)
	oneShotFixer = stampGitHubHITLAsk(t, repos, oneShotFixer.ID, 8005)
	autoReviewerAsk, ok := loops.ReadHITLAsk(autoReviewer.MetadataJSON)
	if !ok || !loops.IsReviewScopeHumanAsk(autoReviewerAsk) {
		t.Fatalf("auto reviewer ask = (%#v, %v), want scope ask", autoReviewerAsk, ok)
	}
	autoFixerAsk, ok := loops.ReadHITLAsk(autoFixer.MetadataJSON)
	if !ok || loops.IsReviewScopeHumanAsk(autoFixerAsk) {
		t.Fatalf("auto fixer ask = (%#v, %v), want preserved agent ask", autoFixerAsk, ok)
	}
	contAsk, ok = loops.ReadHITLAsk(contFixer.MetadataJSON)
	if !ok || contAsk.Question != "Which continuous-manual approach?" {
		t.Fatalf("continuous ask = (%#v, %v)", contAsk, ok)
	}
	oneShotAsk, ok = loops.ReadHITLAsk(oneShotFixer.MetadataJSON)
	if !ok || oneShotAsk.Question != "Which one-shot approach?" {
		t.Fatalf("one-shot ask = (%#v, %v)", oneShotAsk, ok)
	}
	if loops.ReviewFixBudgetLane(autoReviewer) == loops.ReviewFixBudgetLane(contFixer) {
		t.Fatal("automatic and continuous-manual must be different pairing lanes")
	}
	if loops.ReviewFixBudgetLane(oneShotFixer) != "" {
		t.Fatal("one-shot manual must not pair")
	}
	overlaid, err := repos.Loops.GetByID(context.Background(), autoFixer.ID)
	if err != nil || overlaid == nil || !loops.IsReviewScopeHumanHold(*overlaid) {
		t.Fatalf("auto fixer overlay = (%#v, %v), want scope hold", overlaid, err)
	}
	if loops.IsReviewScopeHumanHold(contFixer) || loops.IsReviewScopeHumanHold(oneShotFixer) {
		t.Fatal("other-lane asks must not be overlaid by the automatic scope hold")
	}

	var delivered []string
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(autoReviewer, autoReviewerAsk),
		githubHITLAwaitingFrom(autoFixer, autoFixerAsk),
		githubHITLAwaitingFrom(contFixer, contAsk),
		githubHITLAwaitingFrom(oneShotFixer, oneShotAsk),
	}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
				{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which automatic approach?"},
				{ID: 8003, Author: "looper", Body: "<!-- looper:hitl:ask --> Which continuous-manual approach?"},
				{ID: 8004, Author: "operator", Body: "use approach B"},
				{ID: 8005, Author: "looper", Body: "<!-- looper:hitl:ask --> Which one-shot approach?"},
				{ID: 8006, Author: "operator", Body: "oneshot yes"},
				{ID: 8007, Author: "operator", Body: "Continue"},
			}, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			delivered = append(delivered, loopID+"="+answer)
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		advanceAskPastComment: func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error {
			return advanceSiblingGitHubHITLAsksPastComment(ctx, repos, projectID, repo, prNumber, exceptLoopID, commentID, comments, nil)
		},
	})
	if n != 3 {
		t.Fatalf("delivered = %#v n=%d, want automatic Continue plus other-lane answers", delivered, n)
	}
	if len(delivered) != 3 || delivered[0] != autoReviewer.ID+"=Continue" || delivered[1] != contFixer.ID+"=use approach B" || delivered[2] != oneShotFixer.ID+"=oneshot yes" {
		t.Fatalf("delivered = %#v, want Continue then other-lane answers, not skipped replies", delivered)
	}

	freshCont, err := repos.Loops.GetByID(context.Background(), contFixer.ID)
	if err != nil || freshCont == nil {
		t.Fatalf("continuous fixer get = (%#v, %v)", freshCont, err)
	}
	remainingCont, ok := loops.ReadHITLAsk(freshCont.MetadataJSON)
	if ok && remainingCont.AskCommentID >= 8007 {
		t.Fatalf("continuous AskCommentID = %d, must not advance past other-lane Continue 8007", remainingCont.AskCommentID)
	}
	freshOneShot, err := repos.Loops.GetByID(context.Background(), oneShotFixer.ID)
	if err != nil || freshOneShot == nil {
		t.Fatalf("one-shot fixer get = (%#v, %v)", freshOneShot, err)
	}
	remainingOneShot, ok := loops.ReadHITLAsk(freshOneShot.MetadataJSON)
	if ok && remainingOneShot.AskCommentID >= 8007 {
		t.Fatalf("one-shot AskCommentID = %d, must not advance past other-lane Continue 8007", remainingOneShot.AskCommentID)
	}
	freshAutoFixer, err := repos.Loops.GetByID(context.Background(), autoFixer.ID)
	if err != nil || freshAutoFixer == nil || freshAutoFixer.Status != "awaiting_human" {
		t.Fatalf("auto fixer after Continue = (%#v, %v), want awaiting preserved ask", freshAutoFixer, err)
	}
	remainingAuto, ok := loops.ReadHITLAsk(freshAutoFixer.MetadataJSON)
	if !ok || remainingAuto.AskCommentID != 8002 {
		t.Fatalf("auto fixer AskCommentID = (%#v, %v), want original ordinary ask 8002", remainingAuto, ok)
	}
}

func TestPollGitHubHITLAnswersOnceKeepsLabelWhenRemainingLookupFails(t *testing.T) {
	t.Parallel()
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
	const projectID = "project_remaining_list_fail"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_remaining_list_fail_rev", Seq: 41, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_remaining_list_fail_fix", Seq: 42, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "awaiting_human", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	agentAsk := loops.HITLAsk{
		Kind: "agent_question", Question: "Which approach should Fixer take?",
		Options: []string{"A", "B"}, Status: "awaiting", AskedAt: nowISO,
		Transport: "github", PRNumber: pr, AskCommentID: 8002,
	}
	fixerMeta, err := loops.WriteHITLAsk(nil, agentAsk)
	if err != nil {
		t.Fatalf("WriteHITLAsk(fixer): %v", err)
	}
	fixer.MetadataJSON = &fixerMeta
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr,
		NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md vs PR non-goals before continue.",
	}); err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reviewer = stampGitHubHITLAsk(t, repos, reviewer.ID, 8001)
	fixer = stampGitHubHITLAsk(t, repos, fixer.ID, 8002)
	reviewerAsk, ok := loops.ReadHITLAsk(reviewer.MetadataJSON)
	if !ok {
		t.Fatal("reviewer missing scope ask")
	}
	fixerAsk, ok := loops.ReadHITLAsk(fixer.MetadataJSON)
	if !ok {
		t.Fatal("fixer missing preserved agent ask")
	}

	failingRepos := storage.NewRepositories(failLoopsListQuerier{db: coordinator.DB()})
	if githubHITLPRHasRemainingAwaiting(context.Background(), failingRepos, projectID, repo, pr) != true {
		t.Fatal("remaining lookup must treat a List error as remaining")
	}

	var cleared []int64
	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{
		githubHITLAwaitingFrom(reviewer, reviewerAsk),
		githubHITLAwaitingFrom(fixer, fixerAsk),
	}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 8001, Author: "looper", Body: "<!-- looper:hitl:ask --> Clarify AGENTS.md?"},
				{ID: 8002, Author: "looper", Body: "<!-- looper:hitl:ask --> Which approach?"},
				{ID: 8003, Author: "operator", Body: "Continue"},
			}, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(_ contextType, _ string, gotPR int64, _ string) {
			cleared = append(cleared, gotPR)
		},
		remainingAwaiting: func(ctx contextType, gotRepo string, gotPR int64) bool {
			return githubHITLPRHasRemainingAwaiting(ctx, failingRepos, projectID, gotRepo, gotPR)
		},
	})
	if n != 1 {
		t.Fatalf("delivered = %d, want one pair-level Continue", n)
	}
	freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status != "awaiting_human" {
		t.Fatalf("fixer after Continue = (%#v, %v), want awaiting preserved agent ask", freshFixer, err)
	}
	if len(cleared) != 0 {
		t.Fatalf("cleared = %v, want empty when remaining-ask List fails", cleared)
	}
}

type failLoopsListQuerier struct {
	db *sql.DB
}

func (q failLoopsListQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q failLoopsListQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if strings.Contains(query, "SELECT * FROM loops") && !strings.Contains(query, "WHERE") {
		return nil, errors.New("database is locked")
	}
	return q.db.QueryContext(ctx, query, args...)
}

func (q failLoopsListQuerier) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return q.db.QueryRowContext(ctx, query, args...)
}

func stampGitHubHITLAsk(t *testing.T, repos *storage.Repositories, loopID string, commentID int64) storage.LoopRecord {
	t.Helper()
	fresh, err := repos.Loops.GetByID(context.Background(), loopID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(%s) = (%#v, %v)", loopID, fresh, err)
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok {
		t.Fatalf("loop %s missing HITL ask", loopID)
	}
	ask.Transport = "github"
	ask.AskCommentID = commentID
	if ask.PRNumber == 0 {
		ask.PRNumber = 42
	}
	meta, err := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if err != nil {
		t.Fatalf("WriteHITLAsk(%s): %v", loopID, err)
	}
	fresh.MetadataJSON = &meta
	if err := repos.Loops.Upsert(context.Background(), *fresh); err != nil {
		t.Fatalf("Loops.Upsert(%s): %v", loopID, err)
	}
	return *fresh
}

func githubHITLAwaitingFrom(loop storage.LoopRecord, ask loops.HITLAsk) githubHITLAwaitingLoop {
	repo := ""
	if loop.Repo != nil {
		repo = *loop.Repo
	}
	pr := ask.PRNumber
	if pr == 0 && loop.PRNumber != nil {
		pr = *loop.PRNumber
	}
	return githubHITLAwaitingLoop{
		ID:           loop.ID,
		ProjectID:    loop.ProjectID,
		Repo:         repo,
		Transport:    ask.Transport,
		AskStatus:    ask.Status,
		PRNumber:     pr,
		AskCommentID: ask.AskCommentID,
		BudgetAsk:    githubHITLDecisionOnlyAsk(loop, ask),
		Lane:         loops.ReviewFixBudgetLane(loop),
	}
}

func TestDrainScopeHoldOnStopDrainsPairBeforeScopeStop(t *testing.T) {
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
	const projectID = "project_scope_stop_drain"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_scope_drain_reviewer", Seq: 21, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_drain_fixer", Seq: 22, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reg := NewActiveExecutionRegistry()
	var drained []string
	drain := func(ctx context.Context, loop storage.LoopRecord) error {
		drained = append(drained, loop.ID)
		return drainReviewFixPairExecutions(ctx, repos, loop, reg)
	}
	if _, err := drainScopeHoldOnStop(context.Background(), repos, parked.ID, "Continue", drain); err != nil {
		t.Fatalf("Continue drain: %v", err)
	}
	if len(drained) != 0 {
		t.Fatalf("Continue drained = %v, want none", drained)
	}
	if _, err := drainScopeHoldOnStop(context.Background(), repos, parked.ID, "Stop", drain); err != nil {
		t.Fatalf("Stop drain: %v", err)
	}
	if len(drained) != 1 || drained[0] != parked.ID {
		t.Fatalf("Stop drained = %v, want [%s]", drained, parked.ID)
	}
	if !reg.LoopStopActive(reviewer.ID) || !reg.LoopStopActive(fixer.ID) {
		t.Fatal("pair stop gates not closed after scope Stop drain")
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
	seedPollBudgetQueue(t, repos, nowISO, "project_budget_github", "queue_budget_reviewer", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedPollBudgetQueue(t, repos, nowISO, "project_budget_github", "queue_budget_fixer", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
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
		AskStatus: "awaiting", PRNumber: pr, AskCommentID: 9001, BudgetAsk: true,
	}}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 9001, Author: "looper", Body: posted[0]},
				{ID: 9002, Author: "operator", Body: "unrelated discussion"},
				{ID: 9003, Author: "operator", Body: "Continue"},
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

func TestRecoverGitHubBudgetAskCommentID(t *testing.T) {
	const askedAt = "2026-04-17T12:34:56.000Z"
	marker := githubBudgetAskMarker(11, askedAt)
	comments := []githubAnswerComment{
		{ID: 7001, Author: "looper", Body: marker + "\nfirst ask"},
		{ID: 7002, Author: "operator", Body: "Continue"},
		{ID: 8001, Author: "looper", Body: marker + "\nretry ask"},
		{ID: 9001, Author: "looper", Body: githubBudgetAskMarker(99, askedAt) + "\nother loop"},
	}
	if got := recoverGitHubBudgetAskCommentID(comments[:1], 11, askedAt); got != 7001 {
		t.Fatalf("unanswered ask = %d, want 7001", got)
	}
	if got := recoverGitHubBudgetAskCommentID(comments, 11, askedAt); got != 8001 {
		t.Fatalf("after prior Continue = %d, want unanswered retry ask 8001", got)
	}
	if got := recoverGitHubBudgetAskCommentID(comments, 12, askedAt); got != 0 {
		t.Fatalf("missing loop marker = %d, want 0", got)
	}
	if got := recoverGitHubBudgetAskCommentID(comments[:1], 11, "2026-04-18T00:00:00.000Z"); got != 0 {
		t.Fatalf("prior-cycle unanswered ask = %d, want 0 so a new prompt is posted", got)
	}
	if got := recoverGitHubBudgetAskCommentID(comments[:1], 11, ""); got != 0 {
		t.Fatalf("missing askedAt = %d, want 0", got)
	}
}

func TestGitHubHITLPollRecoversBudgetAskAfterPersistFailure(t *testing.T) {
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
	const projectID = "project_budget_github_recover"
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
		ID: "loop_budget_reviewer_recover", Seq: 11, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_fixer_recover", Seq: 12, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_budget_reviewer_recover", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_budget_fixer_recover", fixer.ID, "fixer", storage.QueuePriorityFixer)

	parked, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	})
	if err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	parkedAsk, ok := loops.ReadHITLAsk(parked.MetadataJSON)
	if !ok {
		t.Fatal("parked loop missing budget ask")
	}
	existingBody := buildGitHubBudgetAskComment(reviewer.Seq, parkedAsk.AskedAt, parkedAsk.Question, parkedAsk.Options, nil)
	var created int
	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
		createComment: func(_ contextType, _ string, _ int64, _ string, _ string) (int64, error) {
			created++
			return 9002, nil
		},
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{{ID: 7001, Author: "looper", Body: existingBody}}, nil
		},
		nowISO: nowISO,
	})
	if delivered != 1 || created != 0 {
		t.Fatalf("deliver after persist-failure = delivered %d created %d, want recover without repost", delivered, created)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Transport != "github" || ask.AskCommentID != 7001 {
		t.Fatalf("recovered ask = %#v, want github comment 7001", ask)
	}

	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: reviewer.ID, ProjectID: projectID, Repo: repo, Transport: "github",
		AskStatus: "awaiting", PRNumber: pr, AskCommentID: 7001, BudgetAsk: true,
	}}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 7001, Author: "looper", Body: existingBody},
				{ID: 7002, Author: "operator", Body: "Continue"},
				{ID: 9002, Author: "looper", Body: "duplicate ask that must not become the recorded id"},
			}, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(_ contextType, _ string, _ int64, _ string) {},
	})
	if n != 1 {
		t.Fatalf("poll delivered = %d, want Continue against recovered ask", n)
	}
	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "queued" {
		t.Fatalf("reviewer after recovered continue = (%#v, %v), want queued", fresh, err)
	}
}

func TestGitHubHITLPollPostsNewAskAfterAPIAnsweredPriorCycle(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, time.April, 17, 12, 34, 56, 0, time.UTC)
	firstISO := now.UTC().Format("2006-01-02T15:04:05.000Z")
	secondISO := now.Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
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
	const projectID = "project_budget_github_generation"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Budget", RepoPath: root, CreatedAt: firstISO, UpdatedAt: firstISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewerMeta := `{"loop":{"iterationCount":8}}`
	reviewer := storage.LoopRecord{
		ID: "loop_budget_reviewer_generation", Seq: 11, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: firstISO, UpdatedAt: firstISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_fixer_generation", Seq: 12, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: firstISO, UpdatedAt: firstISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	seedPollBudgetQueue(t, repos, firstISO, projectID, "queue_budget_reviewer_generation", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedPollBudgetQueue(t, repos, firstISO, projectID, "queue_budget_fixer_generation", fixer.ID, "fixer", storage.QueuePriorityFixer)

	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: firstISO, HITLEnabled: true,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget(first) error = %v", err)
	}
	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	var firstBody string
	if delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
		createComment: func(_ contextType, _ string, _ int64, body, _ string) (int64, error) {
			firstBody = body
			if !strings.Contains(body, githubBudgetAskMarker(reviewer.Seq, firstISO)) {
				t.Fatalf("first ask missing generation marker: %s", body)
			}
			return 7001, nil
		},
		nowISO: firstISO,
	}); delivered != 1 || firstBody == "" {
		t.Fatalf("first deliver = %d body empty=%t, want posted ask", delivered, firstBody == "")
	}

	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
	}
	if _, err := loops.ApplyReviewFixBudgetAnswer(context.Background(), repos, *fresh, loops.ReviewFixBudgetAnswerContinue, firstISO, loops.ReviewFixBudgetLiveCaps{ReviewerMaxPublishes: 8, FixerMaxPushes: 8}); err != nil {
		t.Fatalf("ApplyReviewFixBudgetAnswer(API Continue) error = %v", err)
	}

	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(after continue) = (%#v, %v)", fresh, err)
	}
	reexhaustMeta := `{"loop":{"iterationCount":8}}`
	fresh.MetadataJSON = &reexhaustMeta
	fresh.Status = "running"
	fresh.UpdatedAt = secondISO
	if err := repos.Loops.Upsert(context.Background(), *fresh); err != nil {
		t.Fatalf("Loops.Upsert(re-exhaust) error = %v", err)
	}
	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: *fresh, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: secondISO, HITLEnabled: true,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget(second) error = %v", err)
	}

	all, err = repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List(second) error = %v", err)
	}
	var created int
	var secondBody string
	if delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
		createComment: func(_ contextType, _ string, _ int64, body, _ string) (int64, error) {
			created++
			secondBody = body
			return 8001, nil
		},
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{{ID: 7001, Author: "looper", Body: firstBody}}, nil
		},
		nowISO: secondISO,
	}); delivered != 1 || created != 1 {
		t.Fatalf("second deliver = delivered %d created %d, want a new prompt after API-answered prior cycle", delivered, created)
	}
	if !strings.Contains(secondBody, githubBudgetAskMarker(reviewer.Seq, secondISO)) {
		t.Fatalf("second ask missing current generation marker: %s", secondBody)
	}
	if strings.Contains(secondBody, githubBudgetAskMarker(reviewer.Seq, firstISO)) {
		t.Fatalf("second ask reused prior-cycle marker: %s", secondBody)
	}
	fresh, err = repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil {
		t.Fatalf("Loops.GetByID(second deliver) = (%#v, %v)", fresh, err)
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.AskCommentID != 8001 || ask.AskedAt != secondISO {
		t.Fatalf("second delivered ask = %#v, want comment 8001 at %s", ask, secondISO)
	}
}

func TestGitHubHITLPollDeliversAndStopsReviewFixBudget(t *testing.T) {
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
	const projectID = "project_budget_github_stop"
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
		ID: "loop_budget_reviewer_stop", Seq: 21, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO, MetadataJSON: &reviewerMeta,
	}
	fixer := storage.LoopRecord{
		ID: "loop_budget_fixer_stop", Seq: 22, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_budget_reviewer_stop", reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
	seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_budget_fixer_stop", fixer.ID, "fixer", storage.QueuePriorityFixer)

	if _, err := loops.ParkReviewFixBudget(context.Background(), repos, loops.ParkReviewFixBudgetInput{
		Exhausted: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, Count: 8, Cap: 8, NowISO: nowISO, HITLEnabled: true,
	}); err != nil {
		t.Fatalf("ParkReviewFixBudget() error = %v", err)
	}
	all, err := repos.Loops.List(context.Background())
	if err != nil {
		t.Fatalf("Loops.List() error = %v", err)
	}
	if delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
		createComment: func(_ contextType, _ string, _ int64, _ string, _ string) (int64, error) {
			return 9101, nil
		},
		nowISO: nowISO,
	}); delivered != 1 {
		t.Fatalf("deliverUndeliveredGitHubBudgetAsks() = %d, want 1", delivered)
	}
	terminatedSibling := fixer
	terminatedSibling.Status = "terminated"
	terminatedSibling.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(context.Background(), terminatedSibling); err != nil {
		t.Fatalf("Loops.Upsert(partial sibling terminate) error = %v", err)
	}

	n := pollGitHubHITLAnswersOnce(context.Background(), []githubHITLAwaitingLoop{{
		ID: reviewer.ID, ProjectID: projectID, Repo: repo, Transport: "github",
		AskStatus: "awaiting", PRNumber: pr, AskCommentID: 9101, BudgetAsk: true,
	}}, githubHITLPollDeps{
		listComments: func(_ contextType, _ string, _ int64, _ string) ([]githubAnswerComment, error) {
			return []githubAnswerComment{
				{ID: 9101, Author: "looper", Body: "<!-- looper:hitl:ask v=1 loop=21 -->"},
				{ID: 9102, Author: "operator", Body: "Stop"},
			}, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoop(ctx, repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(_ contextType, _ string, _ int64, _ string) {},
	})
	if n != 1 {
		t.Fatalf("poll delivered = %d, want Stop on reviewer", n)
	}
	fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || fresh == nil || fresh.Status != "terminated" {
		t.Fatalf("reviewer after stop = (%#v, %v), want terminated", fresh, err)
	}
	if _, stillAsked := loops.ReadHITLAsk(fresh.MetadataJSON); stillAsked {
		t.Fatal("reviewer still has a HITL ask after stop")
	}
	sibling, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || sibling == nil || sibling.Status != "terminated" {
		t.Fatalf("fixer after stop = (%#v, %v), want terminated", sibling, err)
	}
}

func TestScopeStopReopensGatesWhenContinueWinsAfterDrain(t *testing.T) {
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
	const projectID = "project_scope_stop_continue_race"
	if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
		ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
	}); err != nil {
		t.Fatalf("Projects.Upsert() error = %v", err)
	}
	repo := "acme/looper"
	pr := int64(42)
	target := "pr:acme/looper:42"
	reviewer := storage.LoopRecord{
		ID: "loop_scope_race_reviewer", Seq: 31, ProjectID: projectID, Type: "reviewer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	fixer := storage.LoopRecord{
		ID: "loop_scope_race_fixer", Seq: 32, ProjectID: projectID, Type: "fixer",
		TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
		Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
	}
	if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
		t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
	}
	if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
		t.Fatalf("Loops.Upsert(fixer) error = %v", err)
	}
	parked, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
		Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
		Question: "Clarify AGENTS.md rule X before unpause",
	})
	if err != nil {
		t.Fatalf("ParkReviewScopeHuman: %v", err)
	}
	reg := NewActiveExecutionRegistry()
	drain := func(ctx context.Context, loop storage.LoopRecord) error {
		return drainReviewFixPairExecutions(ctx, repos, loop, reg)
	}
	afterDrainScopeStopHook = func() {
		fresh, getErr := repos.Loops.GetByID(context.Background(), parked.ID)
		if getErr != nil || fresh == nil {
			t.Errorf("hook GetByID: (%v, %v)", fresh, getErr)
			return
		}
		if _, applyErr := loops.ApplyReviewScopeHumanAnswer(context.Background(), repos, *fresh, "Continue", nowISO); applyErr != nil {
			t.Errorf("racing Continue: %v", applyErr)
			return
		}
		if !reg.LoopStopActive(reviewer.ID) || !reg.LoopStopActive(fixer.ID) {
			t.Error("pair stop gates not closed after drain and racing Continue")
		}
	}
	t.Cleanup(func() { afterDrainScopeStopHook = nil })

	if err := deliverHITLAnswerAfterScopeDrain(context.Background(), repos, coordinator.DB(), nowISO, parked.ID, "Stop", reviewFixBudgetLiveCaps(nil, ""), drain, reg); err != nil {
		t.Fatalf("unapplied Stop deliver: %v", err)
	}
	if reg.LoopStopActive(reviewer.ID) || reg.LoopStopActive(fixer.ID) {
		t.Fatal("pair stop gates still closed after unapplied Stop")
	}
	if _, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: reviewer.ID, RunID: "run_scope_continue", ExecutionID: "exec_scope_continue_rev",
	}); err != nil {
		t.Fatalf("AdmitSpawn reviewer after unapplied Stop: %v", err)
	}
	if _, err := reg.AdmitSpawn(context.Background(), agent.SpawnMeta{
		LoopID: fixer.ID, RunID: "run_scope_continue", ExecutionID: "exec_scope_continue_fix",
	}); err != nil {
		t.Fatalf("AdmitSpawn fixer after unapplied Stop: %v", err)
	}
	freshReviewer, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
	if err != nil || freshReviewer == nil || freshReviewer.Status != "queued" || loops.IsReviewScopeHumanHold(*freshReviewer) {
		t.Fatalf("reviewer after Continue-wins Stop = (%#v, %v), want queued without hold", freshReviewer, err)
	}
	freshFixer, err := repos.Loops.GetByID(context.Background(), fixer.ID)
	if err != nil || freshFixer == nil || freshFixer.Status == "terminated" || loops.IsReviewScopeHumanHold(*freshFixer) {
		t.Fatalf("fixer after Continue-wins Stop = (%#v, %v), want released pair member", freshFixer, err)
	}
}

func TestGitHubScopeAskDeliveryDoesNotClobberContinueOrStop(t *testing.T) {
	for _, tc := range []struct {
		name          string
		race          string
		wantDelivered int
		wantStatus    string
		wantHold      bool
		wantCommentID int64
	}{
		{name: "no_race", wantDelivered: 1, wantStatus: "awaiting_human", wantHold: true, wantCommentID: 9201},
		{name: "continue", race: loops.ReviewFixBudgetAnswerContinue, wantDelivered: 0, wantStatus: "queued", wantHold: false},
		{name: "stop", race: loops.ReviewFixBudgetAnswerStop, wantDelivered: 0, wantStatus: "terminated", wantHold: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			projectID := "project_scope_github_delivery_" + tc.name
			if err := repos.Projects.Upsert(context.Background(), storage.ProjectRecord{
				ID: projectID, Name: "Scope", RepoPath: root, CreatedAt: nowISO, UpdatedAt: nowISO,
			}); err != nil {
				t.Fatalf("Projects.Upsert() error = %v", err)
			}
			repo := "acme/looper"
			pr := int64(42)
			target := "pr:acme/looper:42"
			reviewer := storage.LoopRecord{
				ID: "loop_scope_gh_delivery_rev_" + tc.name, Seq: 301, ProjectID: projectID, Type: "reviewer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "running", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			fixer := storage.LoopRecord{
				ID: "loop_scope_gh_delivery_fix_" + tc.name, Seq: 302, ProjectID: projectID, Type: "fixer",
				TargetType: "pull_request", TargetID: &target, Repo: &repo, PRNumber: &pr,
				Status: "queued", CreatedAt: nowISO, UpdatedAt: nowISO,
			}
			if err := repos.Loops.Upsert(context.Background(), reviewer); err != nil {
				t.Fatalf("Loops.Upsert(reviewer) error = %v", err)
			}
			if err := repos.Loops.Upsert(context.Background(), fixer); err != nil {
				t.Fatalf("Loops.Upsert(fixer) error = %v", err)
			}
			seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_scope_gh_rev_"+tc.name, reviewer.ID, "reviewer", storage.QueuePriorityReviewer)
			seedPollBudgetQueue(t, repos, nowISO, projectID, "queue_scope_gh_fix_"+tc.name, fixer.ID, "fixer", storage.QueuePriorityFixer)
			if _, err := loops.ParkReviewScopeHuman(context.Background(), repos, loops.ParkReviewScopeHumanInput{
				Held: reviewer, Role: "reviewer", Repo: repo, PRNumber: pr, NowISO: nowISO, HITLEnabled: true,
				Question: "Clarify AGENTS.md rule X before unpause",
			}); err != nil {
				t.Fatalf("ParkReviewScopeHuman: %v", err)
			}
			all, err := repos.Loops.List(context.Background())
			if err != nil {
				t.Fatalf("Loops.List() error = %v", err)
			}
			delivered := deliverUndeliveredGitHubBudgetAsks(context.Background(), projectID, all, repos, githubHITLDeliveryDeps{
				createComment: func(ctx contextType, _ string, _ int64, _ string, _ string) (int64, error) {
					if tc.race != "" {
						fresh, err := repos.Loops.GetByID(ctx, reviewer.ID)
						if err != nil || fresh == nil {
							t.Fatalf("GetByID during createComment = (%#v, %v)", fresh, err)
						}
						if _, err := loops.ApplyReviewScopeHumanAnswer(ctx, repos, *fresh, tc.race, nowISO); err != nil {
							t.Fatalf("ApplyReviewScopeHumanAnswer(%s): %v", tc.race, err)
						}
					}
					return 9201, nil
				},
				nowISO: nowISO,
			})
			if delivered != tc.wantDelivered {
				t.Fatalf("deliverUndeliveredGitHubBudgetAsks() = %d, want %d", delivered, tc.wantDelivered)
			}
			fresh, err := repos.Loops.GetByID(context.Background(), reviewer.ID)
			if err != nil || fresh == nil {
				t.Fatalf("Loops.GetByID(reviewer) = (%#v, %v)", fresh, err)
			}
			if fresh.Status != tc.wantStatus {
				t.Fatalf("reviewer status = %s, want %s meta=%s", fresh.Status, tc.wantStatus, derefString(fresh.MetadataJSON))
			}
			if loops.IsReviewScopeHumanHold(*fresh) != tc.wantHold {
				t.Fatalf("reviewer hold = %v, want %v meta=%s", loops.IsReviewScopeHumanHold(*fresh), tc.wantHold, derefString(fresh.MetadataJSON))
			}
			ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
			if tc.wantHold {
				if !ok || !loops.IsReviewScopeHumanAsk(ask) || ask.Transport != "github" || ask.AskCommentID != tc.wantCommentID {
					t.Fatalf("delivered scope ask = (%#v, %v), want github comment %d", ask, ok, tc.wantCommentID)
				}
				return
			}
			if ok && loops.IsReviewScopeHumanAsk(ask) {
				t.Fatalf("stale persist restored scope ask after %s: %#v", tc.race, ask)
			}
		})
	}
}

func seedPollBudgetQueue(t *testing.T, repos *storage.Repositories, nowISO, projectID, id, loopID, queueType string, priority int64) {
	t.Helper()
	if err := repos.Queue.Upsert(context.Background(), storage.QueueItemRecord{
		ID: id, ProjectID: stringPtr(projectID), LoopID: &loopID, Type: queueType,
		TargetType: "pull_request", TargetID: "pr:acme/looper:42", Status: "queued",
		Priority: priority, MaxAttempts: 3, AvailableAt: nowISO, CreatedAt: nowISO, UpdatedAt: nowISO,
		DedupeKey: queueType + ":" + id,
	}); err != nil {
		t.Fatalf("Queue.Upsert(%s) error = %v", id, err)
	}
}
