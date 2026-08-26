package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/disclosure"
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
	return detectGitHubHITLAnswerMatching(comments, askCommentID, answerAuthors, nil)
}

func detectGitHubHITLAnswerMatching(comments []githubAnswerComment, askCommentID int64, answerAuthors []string, accept func(string) bool) string {
	answer, _ := detectGitHubHITLAnswerMatchingWithID(comments, askCommentID, answerAuthors, accept, nil)
	return answer
}

func detectGitHubHITLAnswerMatchingWithID(comments []githubAnswerComment, askCommentID int64, answerAuthors []string, accept func(string) bool, skipIDs map[int64]struct{}) (string, int64) {
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
		if _, skip := skipIDs[c.ID]; skip {
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
		if accept != nil && !accept(body) {
			continue
		}
		if bestID == 0 || c.ID < bestID {
			bestID = c.ID
			answer = body
		}
	}
	return answer, bestID
}

// githubHITLDeliveryDeps are the injected dependencies for posting an undelivered
// review-fix budget ask onto its PR so the answer-poll lane can later consume it.
type githubHITLDeliveryDeps struct {
	createComment func(ctx contextType, repo string, prNumber int64, body, cwd string) (int64, error)
	listComments  func(ctx contextType, repo string, prNumber int64, cwd string) ([]githubAnswerComment, error)
	addLabel      func(ctx contextType, repo string, prNumber int64, label, cwd string)
	projectCWD    func(projectID string) string
	stampBody     func(body, runner string) string
	mentionLogins []string
	awaitingLabel string
	nowISO        string
	logWarn       func(msg string, fields map[string]any)
}

func deliverUndeliveredGitHubBudgetAsks(ctx contextType, projectID string, records []storage.LoopRecord, repos *storage.Repositories, deps githubHITLDeliveryDeps) int {
	if repos == nil || repos.Loops == nil || deps.createComment == nil {
		return 0
	}
	delivered := 0
	awaitingLabel := strings.TrimSpace(deps.awaitingLabel)
	if awaitingLabel == "" {
		awaitingLabel = "looper:awaiting-human"
	}
	for _, loop := range records {
		if loop.ProjectID != projectID || loop.Status != "awaiting_human" {
			continue
		}
		ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
		if !ok || (!loops.IsReviewFixBudgetAsk(ask) && !loops.IsReviewScopeHumanAsk(ask)) {
			continue
		}
		if ask.AskCommentID != 0 && strings.EqualFold(strings.TrimSpace(ask.Transport), "github") {
			continue
		}
		prNumber := ask.PRNumber
		if prNumber == 0 {
			prNumber = derefLoopPRNumber(loop)
		}
		repo := derefLoopRepo(loop)
		if prNumber == 0 || repo == "" {
			continue
		}
		cwd := ""
		if deps.projectCWD != nil {
			cwd = deps.projectCWD(loop.ProjectID)
		}
		commentID, err := recoverOrCreateGitHubBudgetAskComment(ctx, repo, prNumber, cwd, loop, ask, deps)
		if err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github: budget ask delivery failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if commentID == 0 {
			continue
		}
		if deps.addLabel != nil {
			deps.addLabel(ctx, repo, prNumber, awaitingLabel, cwd)
		}
		posted := ask
		persisted, err := persistPairAskDelivery(ctx, repos, loop.ID, deps.nowISO, posted, func(live *loops.HITLAsk) {
			live.Transport = "github"
			live.PRNumber = prNumber
			live.AskCommentID = commentID
		})
		if err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github: budget ask persist failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if !persisted {
			continue
		}
		delivered++
	}
	return delivered
}

// persistPairAskDelivery writes ask-delivery fields onto the live loop when the
// same awaiting pair ask is still current. A racing Continue/Stop can release
// the hold while createComment/sendAsk is in flight; a whole-record upsert of
// the pre-delivery snapshot would restore awaiting_human and scope metadata.
func persistPairAskDelivery(ctx context.Context, repos *storage.Repositories, loopID, nowISO string, posted loops.HITLAsk, apply func(*loops.HITLAsk)) (bool, error) {
	if repos == nil || repos.Loops == nil || strings.TrimSpace(loopID) == "" || apply == nil {
		return false, nil
	}
	unlock := LockLoopRequeue(loopID)
	defer unlock()
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return false, err
	}
	if fresh == nil {
		return false, nil
	}
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*fresh))
	defer unlockTarget()
	current, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return false, err
	}
	if current == nil || current.Status != "awaiting_human" {
		return false, nil
	}
	ask, ok := loops.ReadHITLAsk(current.MetadataJSON)
	if !ok {
		return false, nil
	}
	if !loops.IsReviewFixBudgetAsk(ask) && !loops.IsReviewScopeHumanAsk(ask) {
		return false, nil
	}
	if strings.TrimSpace(ask.Kind) != strings.TrimSpace(posted.Kind) || strings.TrimSpace(ask.AskedAt) != strings.TrimSpace(posted.AskedAt) {
		return false, nil
	}
	status := strings.TrimSpace(ask.Status)
	if strings.EqualFold(status, "answered") || strings.EqualFold(status, "consumed") {
		return false, nil
	}
	apply(&ask)
	meta, err := loops.WriteHITLAsk(current.MetadataJSON, ask)
	if err != nil {
		return false, err
	}
	updated := *current
	updated.MetadataJSON = &meta
	if strings.TrimSpace(nowISO) != "" {
		updated.UpdatedAt = nowISO
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return false, err
	}
	return true, nil
}

func githubBudgetAskMarker(loopSeq int64, askedAt string) string {
	askedAt = strings.TrimSpace(askedAt)
	if askedAt == "" {
		return fmt.Sprintf("<!-- looper:hitl:ask v=1 loop=%d -->", loopSeq)
	}
	return fmt.Sprintf("<!-- looper:hitl:ask v=1 loop=%d askedAt=%s -->", loopSeq, askedAt)
}

func recoverOrCreateGitHubBudgetAskComment(ctx contextType, repo string, prNumber int64, cwd string, loop storage.LoopRecord, ask loops.HITLAsk, deps githubHITLDeliveryDeps) (int64, error) {
	if deps.listComments != nil {
		comments, err := deps.listComments(ctx, repo, prNumber, cwd)
		if err != nil {
			return 0, err
		}
		if recovered := recoverGitHubBudgetAskCommentID(comments, loop.Seq, ask.AskedAt); recovered != 0 {
			return recovered, nil
		}
	}
	if deps.createComment == nil {
		return 0, nil
	}
	body := buildGitHubBudgetAskComment(loop.Seq, ask.AskedAt, ask.Question, ask.Options, deps.mentionLogins)
	if deps.stampBody != nil {
		body = deps.stampBody(body, loop.Type)
	}
	return deps.createComment(ctx, repo, prNumber, body, cwd)
}

func recoverGitHubBudgetAskCommentID(comments []githubAnswerComment, loopSeq int64, askedAt string) int64 {
	askedAt = strings.TrimSpace(askedAt)
	if askedAt == "" {
		return 0
	}
	marker := githubBudgetAskMarker(loopSeq, askedAt)
	bestID := int64(0)
	for _, comment := range comments {
		if !strings.Contains(comment.Body, marker) {
			continue
		}
		if githubBudgetAskAlreadyAnswered(comments, comment.ID) {
			continue
		}
		if bestID == 0 || comment.ID < bestID {
			bestID = comment.ID
		}
	}
	return bestID
}

func githubBudgetAskAlreadyAnswered(comments []githubAnswerComment, askCommentID int64) bool {
	return detectGitHubHITLAnswerMatching(comments, askCommentID, nil, func(body string) bool {
		return loops.IsReviewFixBudgetContinue(body) || loops.IsReviewFixBudgetStop(body)
	}) != ""
}

func buildGitHubBudgetAskComment(loopSeq int64, askedAt, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", githubBudgetAskMarker(loopSeq, askedAt))
	b.WriteString("🤔 **looper needs a decision to continue.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	for _, option := range options {
		if option = strings.TrimSpace(option); option != "" {
			fmt.Fprintf(&b, "\n- %s", option)
		}
	}
	b.WriteString("\n\nReply to this comment with Continue or Stop.")
	if mentions := githubBudgetMentionLine(mentionLogins); mentions != "" {
		b.WriteString("\n\n" + mentions)
	}
	return b.String()
}

func githubBudgetMentionLine(logins []string) string {
	parts := make([]string, 0, len(logins))
	for _, login := range logins {
		login = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(login), "@"))
		if login != "" {
			parts = append(parts, "@"+login)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/cc " + strings.Join(parts, " ")
}

func derefLoopRepo(loop storage.LoopRecord) string {
	if loop.Repo == nil {
		return ""
	}
	return strings.TrimSpace(*loop.Repo)
}

func derefLoopPRNumber(loop storage.LoopRecord) int64 {
	if loop.PRNumber == nil {
		return 0
	}
	return *loop.PRNumber
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
	// remainingAwaiting, when set, is consulted after a successful delivery.
	// clearAwaiting is skipped when another awaiting GitHub ask remains on the PR.
	remainingAwaiting func(ctx contextType, repo string, prNumber int64) bool
	// advanceAskPastComment moves remaining GitHub asks on the same
	// review-fix pair past a consumed Continue/Stop so a later poll cannot
	// treat that comment as an ordinary answer after a scope overlay is released.
	// Residual ordinary asks are advanced only when they have no earlier free-text.
	advanceAskPastComment func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error
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
	BudgetAsk    bool
	// Lane is the review-fix pairing lane (automatic or continuous_manual).
	// Empty means the loop does not pair (one-shot manual).
	Lane string
}

// githubHITLDecisionOnlyAsk reports Continue/Stop-only GitHub answer filtering.
// Scope overlays keep a preserved agent ask; the overlay, not the ask kind, is
// the authority that later Continue/Stop must resolve.
func githubHITLDecisionOnlyAsk(loop storage.LoopRecord, ask loops.HITLAsk) bool {
	return loops.IsReviewFixBudgetAsk(ask) || loops.IsReviewScopeHumanAsk(ask) || loops.IsReviewScopeHumanHold(loop)
}

// githubHITLResidualOrdinaryAsk reports a preserved agent HITL ask that is not
// itself a pair Continue/Stop question. Overlay siblings keep this ask while
// the pair hold is the decision authority.
func githubHITLResidualOrdinaryAsk(ask loops.HITLAsk) bool {
	if loops.IsReviewFixBudgetAsk(ask) || loops.IsReviewScopeHumanAsk(ask) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(ask.Status), "awaiting")
}

// afterDrainScopeStopHook is test-only: runs after a scope Stop drain and before
// the caller applies the answer, so tests can commit a racing Continue.
var afterDrainScopeStopHook func()

// afterAdvanceSiblingGitHubAskListHook is test-only: runs after the sibling List
// snapshot and before per-sibling cursor writes, so tests can commit a racing
// Continue/Stop from another transport.
var afterAdvanceSiblingGitHubAskListHook func()

func drainScopeHoldOnStop(ctx context.Context, repos *storage.Repositories, loopID, answer string, drain func(context.Context, storage.LoopRecord) error) (bool, error) {
	if drain == nil || repos == nil || repos.Loops == nil || !loops.IsReviewFixBudgetStop(answer) {
		return false, nil
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return false, err
	}
	if loop == nil || !loops.IsReviewScopeHumanHold(*loop) {
		return false, nil
	}
	if err := drain(ctx, *loop); err != nil {
		return false, err
	}
	if afterDrainScopeStopHook != nil {
		afterDrainScopeStopHook()
	}
	return true, nil
}

// deliverHITLAnswerAfterScopeDrain drains a live scope pair before applying Stop,
// then reopens sticky spawn gates if that Stop did not remain applied.
func deliverHITLAnswerAfterScopeDrain(ctx context.Context, repos *storage.Repositories, db *sql.DB, nowISO, loopID, answer string, caps loops.ReviewFixBudgetLiveCaps, drain func(context.Context, storage.LoopRecord) error, executions *ActiveExecutionRegistry) error {
	drained, err := drainScopeHoldOnStop(ctx, repos, loopID, answer, drain)
	if err != nil {
		return err
	}
	deliverErr := deliverHITLAnswerToLoopWithCaps(ctx, repos, db, nowISO, loopID, answer, caps, executions)
	if drained {
		ReopenUnappliedScopeStopGates(ctx, repos, executions, loopID)
	}
	return deliverErr
}

func drainReviewFixPairExecutions(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, executions *ActiveExecutionRegistry) error {
	if repos == nil || repos.Loops == nil {
		return nil
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	members := append(loops.FindSiblingReviewFixLoops(all, loop), loop)
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if _, ok := seen[member.ID]; ok {
			continue
		}
		seen[member.ID] = struct{}{}
		if member.ID != loop.ID && !loops.IsReviewFixPairHold(member) {
			continue
		}
		switch strings.TrimSpace(member.Status) {
		case "terminated", "stopped", "completed":
			continue
		}
		if executions != nil {
			if _, err := executions.BeginLoopStop(member.ID, "Stopped by review-fix scope pair"); err != nil {
				return err
			}
		}
	}
	return nil
}

// pollGitHubHITLAnswersOnce runs one pass of the answer-poll lane: for each loop
// waiting on a GitHub HITL answer, it looks for a human's reply after the ask and
// delivers it. It is idempotent — a loop that leaves awaiting_human on delivery
// simply won't be passed in again. A Continue/Stop consumed by a decision-only
// pair member is remembered in-process for this poll and persisted onto siblings.
// Residual ordinary sibling asks keep earlier free-text visible; they skip only
// the consumed decision comment so a standalone ordinary ask can still answer
// Continue or Stop.
func pollGitHubHITLAnswersOnce(ctx contextType, awaiting []githubHITLAwaitingLoop, deps githubHITLPollDeps) int {
	delivered := 0
	consumedDecisionPairs := make(map[string]struct{})
	consumedDecisionComments := make(map[int64]struct{})
	for i, loop := range awaiting {
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
		pairKey := githubHITLDecisionPairKey(loop)
		if loop.BudgetAsk && pairKey != "" {
			if _, consumed := consumedDecisionPairs[pairKey]; consumed {
				continue
			}
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
		var accept func(string) bool
		if loop.BudgetAsk {
			accept = func(body string) bool {
				return loops.IsReviewFixBudgetContinue(body) || loops.IsReviewFixBudgetStop(body)
			}
		}
		answer, commentID := detectGitHubHITLAnswerMatchingWithID(comments, loop.AskCommentID, deps.answerAuthors, accept, consumedDecisionComments)
		if answer == "" {
			continue
		}
		if loop.BudgetAsk && pairKey != "" && commentID != 0 && (loops.IsReviewFixBudgetContinue(answer) || loops.IsReviewFixBudgetStop(answer)) {
			if deps.advanceAskPastComment != nil {
				if err := deps.advanceAskPastComment(ctx, loop.ProjectID, repo, loop.PRNumber, loop.ID, commentID, comments); err != nil {
					if deps.logWarn != nil {
						deps.logWarn("hitl github poll: advance consumed decision failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
					}
					continue
				}
			}
			for j := range awaiting {
				if j == i || githubHITLDecisionPairKey(awaiting[j]) != pairKey {
					continue
				}
				if !awaiting[j].BudgetAsk {
					if residualOrdinaryHasEarlierAnswer(comments, awaiting[j].AskCommentID, commentID, deps.answerAuthors) {
						continue
					}
				}
				if awaiting[j].AskCommentID < commentID {
					awaiting[j].AskCommentID = commentID
				}
			}
			consumedDecisionComments[commentID] = struct{}{}
		}
		if err := deps.deliverAnswer(ctx, loop.ID, answer); err != nil {
			if deps.logWarn != nil {
				deps.logWarn("hitl github poll: deliver answer failed", map[string]any{"loopId": loop.ID, "error": err.Error()})
			}
			continue
		}
		if loop.BudgetAsk && pairKey != "" && (loops.IsReviewFixBudgetContinue(answer) || loops.IsReviewFixBudgetStop(answer)) {
			consumedDecisionPairs[pairKey] = struct{}{}
		}
		if deps.clearAwaiting != nil && (deps.remainingAwaiting == nil || !deps.remainingAwaiting(ctx, repo, loop.PRNumber)) {
			deps.clearAwaiting(ctx, repo, loop.PRNumber, cwd)
		}
		delivered++
	}
	return delivered
}

func residualOrdinaryHasEarlierAnswer(comments []githubAnswerComment, askCommentID, consumedID int64, answerAuthors []string) bool {
	if consumedID == 0 {
		return false
	}
	_, id := detectGitHubHITLAnswerMatchingWithID(comments, askCommentID, answerAuthors, func(body string) bool {
		return !loops.IsReviewFixBudgetContinue(body) && !loops.IsReviewFixBudgetStop(body)
	}, map[int64]struct{}{consumedID: {}})
	return id != 0 && id < consumedID
}

func advanceSiblingGitHubHITLAsksPastComment(ctx context.Context, repos *storage.Repositories, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment, answerAuthors []string) error {
	if repos == nil || repos.Loops == nil || commentID == 0 || prNumber == 0 {
		return nil
	}
	exceptLoopID = strings.TrimSpace(exceptLoopID)
	if exceptLoopID == "" {
		return nil
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	if afterAdvanceSiblingGitHubAskListHook != nil {
		afterAdvanceSiblingGitHubAskListHook()
	}
	var except *storage.LoopRecord
	for i := range all {
		if strings.TrimSpace(all[i].ID) == exceptLoopID {
			except = &all[i]
			break
		}
	}
	if except == nil {
		return nil
	}
	repo = strings.TrimSpace(repo)
	projectID = strings.TrimSpace(projectID)
	for _, loop := range loops.FindSiblingReviewFixLoops(all, *except) {
		if projectID != "" && strings.TrimSpace(loop.ProjectID) != projectID {
			continue
		}
		if repo != "" && derefLoopRepo(loop) != repo {
			continue
		}
		if derefLoopPRNumber(loop) != prNumber {
			continue
		}
		if err := advanceOneSiblingGitHubHITLAskPastComment(ctx, repos, loop.ID, prNumber, commentID, comments, answerAuthors); err != nil {
			return err
		}
	}
	return nil
}

// advanceOneSiblingGitHubHITLAskPastComment writes only AskCommentID onto a
// freshly read sibling. A whole-record upsert of the preceding List snapshot
// would restore awaiting_human and scope-hold metadata after a racing
// Continue/Stop released the overlay.
func advanceOneSiblingGitHubHITLAskPastComment(ctx context.Context, repos *storage.Repositories, loopID string, prNumber, commentID int64, comments []githubAnswerComment, answerAuthors []string) error {
	loopID = strings.TrimSpace(loopID)
	if repos == nil || repos.Loops == nil || loopID == "" || commentID == 0 {
		return nil
	}
	unlock := LockLoopRequeue(loopID)
	defer unlock()
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return nil
	}
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*fresh))
	defer unlockTarget()
	current, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if current == nil || strings.TrimSpace(current.Status) != "awaiting_human" {
		return nil
	}
	ask, ok := loops.ReadHITLAsk(current.MetadataJSON)
	if !ok || !strings.EqualFold(strings.TrimSpace(ask.Transport), "github") {
		return nil
	}
	if ask.PRNumber != 0 && ask.PRNumber != prNumber {
		return nil
	}
	if ask.AskCommentID >= commentID {
		return nil
	}
	if githubHITLResidualOrdinaryAsk(ask) && residualOrdinaryHasEarlierAnswer(comments, ask.AskCommentID, commentID, answerAuthors) {
		return nil
	}
	ask.AskCommentID = commentID
	meta, err := loops.WriteHITLAsk(current.MetadataJSON, ask)
	if err != nil {
		return err
	}
	updated := *current
	updated.MetadataJSON = &meta
	return repos.Loops.Upsert(ctx, updated)
}

func githubHITLDecisionPairKey(loop githubHITLAwaitingLoop) string {
	repo := strings.TrimSpace(loop.Repo)
	lane := strings.TrimSpace(loop.Lane)
	if repo == "" || loop.PRNumber == 0 || lane == "" {
		return ""
	}
	return fmt.Sprintf("%s#%d:%s", repo, loop.PRNumber, lane)
}

func githubHITLPRHasRemainingAwaiting(ctx context.Context, repos *storage.Repositories, projectID, repo string, prNumber int64) bool {
	if repos == nil || repos.Loops == nil || prNumber == 0 {
		return false
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		// Unknown remaining state must keep looper:awaiting-human. A transient
		// List error after one delivery must not clear the label while another
		// GitHub-backed ask may still be open on the PR.
		return true
	}
	repo = strings.TrimSpace(repo)
	for _, loop := range all {
		if strings.TrimSpace(projectID) != "" && loop.ProjectID != projectID {
			continue
		}
		if loop.Status != "awaiting_human" {
			continue
		}
		ask, ok := loops.ReadHITLAsk(loop.MetadataJSON)
		if !ok {
			continue
		}
		if s := strings.TrimSpace(ask.Status); s != "" && s != "awaiting" {
			continue
		}
		askPR := ask.PRNumber
		if askPR == 0 {
			askPR = derefLoopPRNumber(loop)
		}
		if askPR != prNumber {
			continue
		}
		if repo != "" {
			loopRepo := derefLoopRepo(loop)
			if loopRepo != "" && !strings.EqualFold(loopRepo, repo) {
				continue
			}
		}
		transport := strings.TrimSpace(ask.Transport)
		if transport != "" && !strings.EqualFold(transport, "github") {
			continue
		}
		return true
	}
	return false
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
// answer, a message does NOT resolve a pending mid-run ask — the agent reads it
// and decides whether to proceed, answer, or ask again. A review-fix budget ask
// is the exception: only Continue/Stop may unpark, and they apply the budget
// decision instead of enqueueing conversational text.
func enqueueHumanMessageToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, text string) error {
	return enqueueHumanMessageToLoopWithCaps(ctx, repos, nil, nowISO, loopID, text, reviewFixBudgetLiveCaps(nil, ""), nil)
}

func enqueueHumanMessageToLoopWithCaps(ctx context.Context, repos *storage.Repositories, db *sql.DB, nowISO, loopID, text string, caps loops.ReviewFixBudgetLiveCaps, executions *ActiveExecutionRegistry) error {
	// Share process-wide requeue exclusion with API discard+retry so free-text
	// inbox delivery cannot requeue paused/waiting/manual_intervention loops
	// between discard preflight and git reset (see LockLoopRequeue).
	// Call order: per-loop lock first, then same-target lock (matches API).
	unlock := LockLoopRequeue(loopID)
	defer unlock()

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	switch loop.Status {
	case "completed", "failed", "stopped", "terminated", "human_takeover":
		return nil
	}
	// Same-target exclusion: a different waiting loop on this PR/issue can
	// otherwise requeue while discard+retry holds only that other loop's
	// per-loop mutex and wipes the shared worktree before the retry TX.
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
	defer unlockTarget()

	// Budget/scope pair holds (HITL ask or no-ask pause) are never conversational
	// inbox turns. Only explicit Continue/Stop may unpark.
	if loops.IsReviewFixBudgetHold(*loop) {
		if loops.IsReviewFixBudgetContinue(text) || loops.IsReviewFixBudgetStop(text) {
			return applyReviewFixBudgetAnswerTX(ctx, db, repos, *loop, text, nowISO, caps)
		}
		return nil
	}
	if loops.IsReviewScopeHumanHold(*loop) {
		if loops.IsReviewFixBudgetContinue(text) || loops.IsReviewFixBudgetStop(text) {
			return applyReviewScopeHumanAnswerAndReopen(ctx, db, repos, executions, *loop, text, nowISO)
		}
		if feishuOverlayResidualCardIsNotPairDecision(*loop, text) {
			write := func(writeRepos *storage.Repositories) error {
				return persistOverlayResidualAskAnswer(ctx, writeRepos, loopID, text, nowISO)
			}
			if db != nil {
				return storage.WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
					return write(storage.NewRepositories(tx))
				})
			}
			return write(repos)
		}
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

func reviewFixBudgetLiveCaps(cfg *config.Config, projectID string) loops.ReviewFixBudgetLiveCaps {
	if cfg == nil {
		return loops.ReviewFixBudgetLiveCaps{
			ReviewerMaxPublishes: config.DefaultReviewFixBudgetCap,
			FixerMaxPushes:       config.DefaultReviewFixBudgetCap,
		}
	}
	roles := config.ProjectRoleConfigs(*cfg, projectID)
	return loops.ReviewFixBudgetLiveCaps{
		ReviewerMaxPublishes: roles.Reviewer.Behavior.Loop.MaxPublishesPerPR,
		FixerMaxPushes:       roles.Fixer.Behavior.Loop.MaxPushesPerPR,
	}
}

func applyReviewFixBudgetAnswerTX(ctx context.Context, db *sql.DB, repos *storage.Repositories, loop storage.LoopRecord, answer, nowISO string, caps loops.ReviewFixBudgetLiveCaps) error {
	if db != nil {
		return storage.WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
			txRepos := storage.NewRepositories(tx)
			fresh, err := txRepos.Loops.GetByID(ctx, loop.ID)
			if err != nil {
				return err
			}
			if fresh == nil {
				return nil
			}
			_, err = loops.ApplyReviewFixBudgetAnswer(ctx, txRepos, *fresh, answer, nowISO, caps)
			return err
		})
	}
	_, err := loops.ApplyReviewFixBudgetAnswer(ctx, repos, loop, answer, nowISO, caps)
	return err
}

func applyReviewScopeHumanAnswerTX(ctx context.Context, db *sql.DB, repos *storage.Repositories, loop storage.LoopRecord, answer, nowISO string) (loops.ReviewScopeHumanAnswerResult, error) {
	if db != nil {
		var result loops.ReviewScopeHumanAnswerResult
		err := storage.WithTransaction(ctx, db, nil, func(tx *sql.Tx) error {
			txRepos := storage.NewRepositories(tx)
			fresh, err := txRepos.Loops.GetByID(ctx, loop.ID)
			if err != nil {
				return err
			}
			if fresh == nil {
				return nil
			}
			result, err = loops.ApplyReviewScopeHumanAnswer(ctx, txRepos, *fresh, answer, nowISO)
			return err
		})
		return result, err
	}
	return loops.ApplyReviewScopeHumanAnswer(ctx, repos, loop, answer, nowISO)
}

func applyReviewScopeHumanAnswerAndReopen(ctx context.Context, db *sql.DB, repos *storage.Repositories, executions *ActiveExecutionRegistry, loop storage.LoopRecord, answer, nowISO string) error {
	result, err := applyReviewScopeHumanAnswerTX(ctx, db, repos, loop, answer, nowISO)
	if err != nil {
		return err
	}
	if loops.IsReviewFixBudgetStop(answer) && !result.Applied {
		ReopenUnappliedScopeStopGates(ctx, repos, executions, loop.ID)
	}
	return nil
}

// ReopenUnappliedScopeStopGates clears sticky pair spawn gates left by a Stop
// drain when ApplyReviewScopeHumanAnswer did not apply (Continue already
// released the hold). Terminal members stay gated.
func ReopenUnappliedScopeStopGates(ctx context.Context, repos *storage.Repositories, executions *ActiveExecutionRegistry, loopID string) {
	if executions == nil || repos == nil || repos.Loops == nil || strings.TrimSpace(loopID) == "" {
		return
	}
	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return
	}
	if loops.IsReviewScopeHumanHold(*loop) {
		return
	}
	members := []storage.LoopRecord{*loop}
	if all, listErr := repos.Loops.List(ctx); listErr == nil {
		members = append(loops.FindSiblingReviewFixLoops(all, *loop), *loop)
	}
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if _, ok := seen[member.ID]; ok {
			continue
		}
		seen[member.ID] = struct{}{}
		switch strings.TrimSpace(member.Status) {
		case "terminated", "stopped", "completed":
			continue
		}
		executions.ClearLoopStop(member.ID)
	}
}

func deliverHITLAnswerToLoop(ctx context.Context, repos *storage.Repositories, nowISO, loopID, answer string) error {
	return deliverHITLAnswerToLoopWithCaps(ctx, repos, nil, nowISO, loopID, answer, reviewFixBudgetLiveCaps(nil, ""), nil)
}

func deliverHITLAnswerToLoopWithCaps(ctx context.Context, repos *storage.Repositories, db *sql.DB, nowISO, loopID, answer string, caps loops.ReviewFixBudgetLiveCaps, executions *ActiveExecutionRegistry) error {
	// Same requeue + target exclusion as free-text enqueue / API discard+retry.
	unlock := LockLoopRequeue(loopID)
	defer unlock()

	loop, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil || loop == nil {
		return err
	}
	if loop.Status != "awaiting_human" && !loops.IsReviewFixPairHold(*loop) {
		return nil
	}
	unlockTarget := LockLoopTarget(LoopTargetGuardKeyFromRecord(*loop))
	defer unlockTarget()
	if loops.IsReviewFixBudgetHold(*loop) {
		// Budget Continue/Stop (HITL ask or no-ask hold) — fail-closed TX when DB set.
		return applyReviewFixBudgetAnswerTX(ctx, db, repos, *loop, answer, nowISO, caps)
	}
	if loops.IsReviewScopeHumanHold(*loop) {
		return applyReviewScopeHumanAnswerAndReopen(ctx, db, repos, executions, *loop, answer, nowISO)
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
	awaitingLabel := "looper:awaiting-human"
	var mentionLogins []string
	var answerAuthors []string
	if gh := input.Config.HITL.GitHub; gh != nil {
		if strings.TrimSpace(gh.AwaitingLabel) != "" {
			awaitingLabel = strings.TrimSpace(gh.AwaitingLabel)
		}
		mentionLogins = gh.MentionLogins
		answerAuthors = gh.AnswerAuthors
	}
	nowISO := eventlog.FormatJavaScriptISOString(input.Now().UTC())
	deliveryDeps := githubHITLDeliveryDeps{
		createComment: func(ctx contextType, repo string, pr int64, body, cwd string) (int64, error) {
			res, err := input.GitHubGateway.CreateIssueComment(ctx, githubinfra.IssueCommentInput{Repo: repo, IssueNumber: pr, Body: body, CWD: cwd})
			if err != nil {
				return 0, err
			}
			return res.ID, nil
		},
		listComments: func(ctx contextType, repo string, pr int64, cwd string) ([]githubAnswerComment, error) {
			cs, err := input.GitHubGateway.ListIssueComments(ctx, githubinfra.ViewIssueInput{Repo: repo, IssueNumber: pr, CWD: cwd})
			if err != nil {
				return nil, err
			}
			out := make([]githubAnswerComment, 0, len(cs))
			for _, c := range cs {
				out = append(out, githubAnswerComment{ID: c.ID, Author: c.Author, Body: c.Body})
			}
			return out, nil
		},
		addLabel: func(ctx contextType, repo string, pr int64, label, cwd string) {
			_ = input.GitHubGateway.AddPullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: repo, PRNumber: pr, Labels: []string{label}, CWD: cwd})
		},
		projectCWD: func(string) string { return project.RepoPath },
		stampBody: func(body, runner string) string {
			return disclosure.FromConfig(*input.Config).Markdown(body, runner, disclosure.ChannelIssueComment)
		},
		mentionLogins: mentionLogins,
		awaitingLabel: awaitingLabel,
		nowISO:        nowISO,
	}
	if input.Logger != nil {
		deliveryDeps.logWarn = func(msg string, fields map[string]any) { input.Logger.Warn(msg, fields) }
	}
	if deliverUndeliveredGitHubBudgetAsks(ctx, project.ID, allLoops, input.Repos, deliveryDeps) > 0 {
		allLoops, err = input.Repos.Loops.List(ctx)
		if err != nil {
			return
		}
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
			// Scope asks and scope overlays share Continue/Stop-only filtering
			// with budget asks so unrelated PR chatter does not poison the
			// decision. Overlay siblings keep a preserved agent ask kind.
			BudgetAsk: githubHITLDecisionOnlyAsk(l, ask),
			Lane:      loops.ReviewFixBudgetLane(l),
		})
	}
	if len(awaiting) == 0 {
		return
	}

	gw := input.GitHubGateway

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
			return deliverHITLAnswerAfterScopeDrain(ctx, input.Repos, input.DB, nowISO, loopID, answer, reviewFixBudgetLiveCaps(input.Config, project.ID), input.DrainHITLPair, input.ActiveExecutions)
		},
		clearAwaiting: func(ctx contextType, repo string, pr int64, cwd string) {
			_ = gw.RemovePullRequestLabels(ctx, githubinfra.PullRequestLabelsInput{Repo: repo, PRNumber: pr, Labels: []string{awaitingLabel}, CWD: cwd})
		},
		remainingAwaiting: func(ctx contextType, repo string, pr int64) bool {
			return githubHITLPRHasRemainingAwaiting(ctx, input.Repos, project.ID, repo, pr)
		},
		advanceAskPastComment: func(ctx contextType, projectID, repo string, prNumber int64, exceptLoopID string, commentID int64, comments []githubAnswerComment) error {
			return advanceSiblingGitHubHITLAsksPastComment(ctx, input.Repos, projectID, repo, prNumber, exceptLoopID, commentID, comments, answerAuthors)
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
