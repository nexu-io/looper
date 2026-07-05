package runtime

import (
	"context"
	"strings"

	"github.com/nexu-io/looper/internal/eventlog"
	githubinfra "github.com/nexu-io/looper/internal/infra/github"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

type contextType = context.Context

// githubAnswerComment is the minimal shape the HITL answer detector needs from a
// PR issue comment.
type githubAnswerComment struct {
	ID     int64
	Author string
	Body   string
}

// looperCommentMarker tags every comment looper itself posts (the ask marker and
// the disclosure stamp both start with it), so a comment carrying it is
// bot-authored and can never be mistaken for a human answer — this is robust even
// when the bot and a human share the same GitHub account.
const looperCommentMarker = "<!-- looper:"

// detectGitHubHITLAnswer returns the human's answer to a GitHub HITL ask, or ""
// when none has arrived yet. The answer is the FIRST comment posted after the ask
// (comment id > askCommentID; GitHub comment ids are monotonic) that is NOT one of
// looper's own comments (no looper marker). When answerAuthors is non-empty the
// commenter must be on that allowlist; otherwise any human reply may answer.
// Empty-bodied comments are ignored so ordinary reactions/edits don't count.
func detectGitHubHITLAnswer(comments []githubAnswerComment, askCommentID int64, answerAuthors []string) string {
	allow := make(map[string]bool, len(answerAuthors))
	for _, a := range answerAuthors {
		if a = strings.TrimSpace(a); a != "" {
			allow[strings.ToLower(a)] = true
		}
	}
	bestID := int64(0)
	answer := ""
	for _, c := range comments {
		if c.ID <= askCommentID {
			continue
		}
		if strings.Contains(c.Body, looperCommentMarker) {
			continue // looper's own comment (ask / progress / decision-log), never an answer
		}
		author := strings.TrimSpace(c.Author)
		if author == "" {
			continue
		}
		if len(allow) > 0 && !allow[strings.ToLower(author)] {
			continue
		}
		body := strings.TrimSpace(c.Body)
		if body == "" {
			continue
		}
		if bestID == 0 || c.ID < bestID {
			bestID = c.ID
			answer = body
		}
	}
	return answer
}

// githubHITLPollDeps are the injected dependencies of the answer-poll lane, kept
// as functions so the lane is testable and decoupled from the scheduler wiring.
type githubHITLPollDeps struct {
	// listComments returns a PR's issue comments (oldest-first is fine; the
	// detector orders by id).
	listComments func(ctx contextType, repo string, prNumber int64, cwd string) ([]githubAnswerComment, error)
	// deliverAnswer feeds the human's answer into the shared HITL core (flips the
	// loop to running + requeues for resume). Wired to the api handler.
	deliverAnswer func(ctx contextType, loopID, answer string) error
	// clearAwaiting removes the awaiting-human label from the PR after delivery.
	clearAwaiting func(ctx contextType, repo string, prNumber int64, cwd string)
	// projectCWD returns the local repo path for a project (gh runs there).
	projectCWD    func(projectID string) string
	answerAuthors []string
	logWarn       func(msg string, fields map[string]any)
}

// githubHITLAwaitingLoop is the minimal loop shape the lane needs.
type githubHITLAwaitingLoop struct {
	ID           string
	ProjectID    string
	Repo         string
	Transport    string
	AskStatus    string
	PRNumber     int64
	AskCommentID int64
}

// pollGitHubHITLAnswersOnce runs one pass of the answer-poll lane: for each loop
// waiting on a GitHub HITL answer, it looks for a human's reply after the ask and
// delivers it. It is idempotent — a loop that leaves awaiting_human on delivery
// simply won't be passed in again.
func pollGitHubHITLAnswersOnce(ctx contextType, loops []githubHITLAwaitingLoop, deps githubHITLPollDeps) int {
	delivered := 0
	for _, loop := range loops {
		if !strings.EqualFold(strings.TrimSpace(loop.Transport), "github") || loop.PRNumber == 0 {
			continue
		}
		if s := strings.TrimSpace(loop.AskStatus); s != "" && s != "awaiting" {
			continue
		}
		repo := strings.TrimSpace(loop.Repo)
		if repo == "" {
			continue
		}
		cwd := ""
		if deps.projectCWD != nil {
			cwd = deps.projectCWD(loop.ProjectID)
		}
		comments, err := deps.listComments(ctx, repo, loop.PRNumber, cwd)
		if err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github poll: list comments failed", map[string]any{"loopId": loop.ID, "repo": repo, "pr": loop.PRNumber, "error": err.Error()})
			}
			continue
		}
		answer := detectGitHubHITLAnswer(comments, loop.AskCommentID, deps.answerAuthors)
		if answer == "" {
			continue
		}
		if err := deps.deliverAnswer(ctx, loop.ID, answer); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github poll: deliver answer failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if deps.clearAwaiting != nil {
			deps.clearAwaiting(ctx, repo, loop.PRNumber, cwd)
		}
		delivered++
	}
	return delivered
}

// deliverHITLAnswerToLoop is the runtime-side equivalent of the api
// handler's deliverHumanAnswer for the poll lane: it stores the human's answer on
// an awaiting_human loop, flips it back to running, and requeues the queue item
// that suspendForHuman cancelled — so the worker resumes with the answer.
// enqueueHumanMessageToLoop queues a free-text human message for a loop and makes
// sure it gets consumed soon: a loop that isn't actively running is nudged to
// queued so the scheduler picks it up and the worker drains the message on its
// next turn; a running loop drains it when the current turn ends. Terminal loops
// are left alone (a message can't reopen a finished loop yet). Unlike a button
// answer, a message does NOT resolve a pending ask — the agent reads it and
// decides whether to proceed, answer, or ask again.
func enqueueHumanMessageToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, text string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	switch loop.Status {
	case "completed", "failed", "stopped", "terminated", "human_takeover":
		return nil
	}
	meta, werr := loops.AppendHumanMessage(loop.MetadataJSON, loops.HumanMessage{At: nowISO, Text: text})
	if werr != nil {
		return werr
	}
	updated := *loop
	updated.MetadataJSON = &meta
	updated.UpdatedAt = nowISO
	notRunning := loop.Status != "running"
	if notRunning {
		// Wake it so the message is consumed ASAP; a running loop keeps running and
		// drains on its next turn.
		updated.Status = "queued"
		updated.NextRunAt = &nowISO
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	if notRunning {
		_, err = repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	}
	return err
}

func deliverHITLAnswerToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, answer string) error {
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	if loop.Status != "awaiting_human" {
		return nil
	}
	ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
	if !ok {
		return nil
	}
	ask.Answer = answer
	ask.Status = "answered"
	ask.AnsweredAt = nowISO
	meta, werr := loops.WriteHITLAsk(loop.MetadataJSON, ask)
	if werr != nil {
		return werr
	}
	updated := *loop
	updated.MetadataJSON = &meta
	updated.Status = "running"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	_, err = repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	return err
}

// runGitHubHITLPoll runs one answer-poll pass for a project's awaiting_human
// loops that carry a GitHub ask. Gated by hitl.enabled + the github transport;
// a no-op otherwise.
func runGitHubHITLPoll(ctx context.Context, input defaultSchedulerTickInput, project storage.ProjectRecord) {
	if input.Config == nil || !input.Config.HITL.Enabled || input.GitHubGateway == nil || input.Repos == nil {
		return
	}
	transport := strings.TrimSpace(strings.ToLower(input.Config.HITL.AnswerTransport))
	if transport != "" && transport != "github" {
		return
	}

	allLoops, err := input.Repos.Loops.List(ctx)
	if err != nil {
		return
	}
	awaiting := make([]githubHITLAwaitingLoop, 0)
	for _, l := range allLoops {
		if l.ProjectID != project.ID || l.Status != "awaiting_human" {
			continue
		}
		ask, ok := loops.ReadHITLAsk(l.MetadataJSON)
		if !ok || !strings.EqualFold(strings.TrimSpace(ask.Transport), "github") || ask.PRNumber == 0 {
			continue
		}
		repo := ""
		if l.Repo != nil {
			repo = *l.Repo
		}
		awaiting = append(awaiting, githubHITLAwaitingLoop{
			ID: l.ID, ProjectID: l.ProjectID, Repo: repo,
			Transport: ask.Transport, AskStatus: ask.Status, PRNumber: ask.PRNumber, AskCommentID: ask.AskCommentID,
		})
	}
	if len(awaiting) == 0 {
		return
	}

	awaitingLabel := "looper:awaiting-human"
	var answerAuthors []string
	if gh := input.Config.HITL.GitHub; gh != nil {
		if strings.TrimSpace(gh.AwaitingLabel) != "" {
			awaitingLabel = strings.TrimSpace(gh.AwaitingLabel)
		}
		answerAuthors = gh.AnswerAuthors
	}
	gw := input.GitHubGateway
	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())

	deps := githubHITLPollDeps{
		listComments: func(ctx contextType, repo string, pr int64, cwd string) ([]githubAnswerComment, error) {
			cs, err := gw.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: pr, CWD: cwd})
			if err != nil {
				return nil, err
			}
			out := make([]githubAnswerComment, 0, len(cs))
			for _, c := range cs {
				out = append(out, githubAnswerComment{ID: c.ID, Author: c.Author, Body: c.Body})
			}
			return out, nil
		},
		deliverAnswer: func(ctx contextType, loopID, answer string) error {
			return deliverHITLAnswerToLoop(ctx, input.Repos, nowISO, loopID, answer)
		},
		clearAwaiting: func(ctx contextType, repo string, pr int64, cwd string) {
			_ = gw.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: repo, PRNumber: pr, Labels: []string{awaitingLabel}, CWD: cwd})
		},
		projectCWD:    func(string) string { return project.RepoPath },
		answerAuthors: answerAuthors,
	}
	if input.Logger != nil {
		deps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}

	if n := pollGitHubHITLAnswersOnce(ctx, awaiting, deps); n > 0 && input.Logger != nil {
		input.Logger.Info("hitl github: delivered human answers", map[string]any{"projectId": project.ID, "count": n})
	}
}
