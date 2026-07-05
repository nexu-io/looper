package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// hitlSentinelRelPath is where an agent writes a mid-run question, relative to
// the worktree root. Mirrors synclo's afk ask sentinel.
const hitlSentinelRelPath = ".looper/ask.json"

// hitlPromptInstruction is appended to the worker prompt ONLY when hitl.enabled
// is true. It tells the agent how to pause and ask a human instead of guessing.
const hitlPromptInstruction = `

---
HUMAN-IN-THE-LOOP: You are trusted to make implementation decisions yourself. Being ABLE to form a reasonable recommendation is NOT a reason to ask a human — it is a reason to PROCEED. Do your homework (read the codebase and context), pick the best option, and carry on. State what you chose and why in your PR description so a human can course-correct in review — that review IS the checkpoint for reversible calls.

Escalate to a human — by writing a JSON file at .looper/ask.json in the repository root and then STOPPING — ONLY when one of these genuinely holds:
  1. You cannot form a confident recommendation: the options are a real toss-up, or the choice hinges on information only a human has (product intent, private context, an unstated requirement).
  2. The action is high-stakes or hard to reverse: destructive or data-losing, security- or privacy-sensitive, a public or contractual commitment, or it spends real money — a human should sign off even when you have an opinion.
  3. It is a direction or strategy call, not an implementation detail.
For everything else — naming, file contents, formats, defaults, structure, which reversible approach to take — DECIDE and proceed. Do not ask.

When you DO escalate, make it a decision brief the human can confirm in seconds (not raw research):
{
  "question": "<one concise question>",
  "options": ["<option 1>", "<option 2>"],
  "recommendation": "<1-2 sentences: what you found, what you'd do, and why>",
  "recommendedOption": "<the option you recommend, matching one of options>",
  "consequences": {"<option 1>": "<what happens if picked>", "<option 2>": "<what happens if picked>"},
  "confidence": "<high|medium|low>"
}
question + options are required; the rest are strongly encouraged (a bare question with no homework is a poor ask). Then STOP immediately without making further changes. A human will answer and you will be resumed in this same session with their decision.
---`

type hitlAsk struct {
	Question          string            `json:"question"`
	Options           []string          `json:"options"`
	Recommendation    string            `json:"recommendation,omitempty"`
	RecommendedOption string            `json:"recommendedOption,omitempty"`
	Consequences      map[string]string `json:"consequences,omitempty"`
	Confidence        string            `json:"confidence,omitempty"`
}

// consumeAskSentinel reads and removes the agent's ask sentinel from the
// worktree, if present. Returns (nil, nil) when no sentinel exists. Consuming
// (deleting) it prevents the same question from re-suspending on resume.
func consumeAskSentinel(worktreePath string) (*hitlAsk, error) {
	if strings.TrimSpace(worktreePath) == "" {
		return nil, nil
	}
	path := filepath.Join(worktreePath, hitlSentinelRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var ask hitlAsk
	if err := json.Unmarshal(raw, &ask); err != nil {
		// A malformed sentinel is not a hard failure: remove it and ignore.
		_ = os.Remove(path)
		return nil, nil
	}
	_ = os.Remove(path)
	if strings.TrimSpace(ask.Question) == "" {
		return nil, nil
	}
	return &ask, nil
}

// awaitingHumanError is returned from the execute step when the agent asked a
// human mid-run. The step loop catches it and suspends the loop as
// awaiting_human instead of treating it as a failure.
type awaitingHumanError struct {
	question    string
	options     []string
	sessionID   string
	executionID string
	vendor      string
	// The agent's decision brief (optional) — carried through to the ask card.
	recommendation    string
	recommendedOption string
	consequences      map[string]string
	confidence        string
}

func (e *awaitingHumanError) Error() string { return "worker paused awaiting human decision" }

func asAwaitingHumanError(err error) (*awaitingHumanError, bool) {
	var typed *awaitingHumanError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

// pendingHumanAnswer returns a resume prompt + native session id when the loop
// carries a human answer to a prior mid-run question. It is READ-ONLY: it does
// NOT mark the answer consumed. That matters because a resumed agent turn can
// fail or time out before completing — leaving the answer "answered" lets the
// retry re-read and re-deliver it instead of silently dropping the human's
// decision. The answer is flipped to "consumed" only once the turn completes,
// via markHumanAnswerConsumed. Returns empty strings when no answer is pending.
// Only called when hitl.enabled is true.
// pendingTakeoverResume returns the native session id (+ a continue prompt) a
// human drove during an interactive takeover that has since been handed back, so
// the daemon's next worker run resumes THAT session and sees their turns. Empty
// when no takeover resume is pending. Independent of hitl.enabled.
func (r *Runner) pendingTakeoverResume(loop *storage.LoopRecord) (string, string) {
	tr, ok := loops.ReadTakeoverResume(loop.MetadataJSON)
	if !ok || strings.TrimSpace(tr.SessionID) == "" {
		return "", ""
	}
	prompt := strings.TrimSpace(tr.Prompt)
	if prompt == "" {
		prompt = "A human took this task's agent session over directly and has handed it back. Review the whole conversation so far — including their turns — and continue from where they left off; do not restart from scratch."
	}
	return prompt, tr.SessionID
}

// latestNativeSessionID returns the loop's most recent captured agent session id,
// so a mailbox-driven turn can native-resume the SAME session and have the full
// conversation context. Empty when none is recorded.
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
}

// clearHumanInbox drops the loop's drained human messages after a successful turn
// so they are not re-injected on a later run. No-op when the inbox is empty.
func (r *Runner) clearHumanInbox(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	if len(loops.ReadHumanInbox(fresh.MetadataJSON)) == 0 {
		return
	}
	meta, werr := loops.ClearHumanInbox(fresh.MetadataJSON)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

// markTakeoverResumeConsumed clears the takeover-resume marker after a successful
// resumed turn so it is not re-applied on later runs. No-op when absent.
func (r *Runner) markTakeoverResumeConsumed(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	if _, ok := loops.ReadTakeoverResume(fresh.MetadataJSON); !ok {
		return
	}
	meta, werr := loops.ClearTakeoverResume(fresh.MetadataJSON)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	resumePrompt := fmt.Sprintf("A human answered the question you asked earlier (%q). Their decision: %s\nContinue the task using this decision; do not ask the same question again.", ask.Question, ask.Answer)
	return resumePrompt, ask.SessionID
}

// markHumanAnswerConsumed flips a delivered human answer from "answered" to
// "consumed" so it is not re-injected on any later run of the same loop. It is
// called only after the resumed agent turn completes successfully. No-op when
// there is no answered ask. Persists against the freshest loop record so it does
// not clobber concurrent metadata writes. Only called when hitl.enabled is true.
func (r *Runner) markHumanAnswerConsumed(ctx context.Context, loop *storage.LoopRecord) {
	if r.repos == nil || r.repos.Loops == nil {
		return
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return
	}
	ask.Status = "consumed"
	meta, werr := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if werr != nil {
		return
	}
	fresh.MetadataJSON = &meta
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err == nil {
		loop.MetadataJSON = &meta
	}
}

// readFreshHITLAsk reads the loop's HITL ask from the freshest persisted record,
// falling back to the in-memory copy when the store is unavailable.
func (r *Runner) readFreshHITLAsk(ctx context.Context, loop *storage.LoopRecord) (loops.HITLAsk, bool) {
	meta := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if got, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && got != nil {
			meta = got.MetadataJSON
		}
	}
	return loops.ReadHITLAsk(meta)
}

// detectHumanAsk consumes the agent's ask sentinel (if any) and, when present,
// returns a typed awaitingHumanError carrying the question, options, and the
// agent's native session id (so the run can resume the same session).
func (r *Runner) detectHumanAsk(ctx context.Context, input stepInput, worktreePath, executionID string) (*awaitingHumanError, error) {
	ask, err := consumeAskSentinel(worktreePath)
	if err != nil {
		// Best-effort: a read error is treated as "no ask" rather than failing the
		// run, but surface it — a present-but-unreadable sentinel means the agent
		// wanted to ask a human and we're about to proceed as if it didn't.
		if r.logger != nil {
			r.logger.Warn("worker could not read HITL ask sentinel; proceeding as no ask", map[string]any{
				"loopId": input.Loop.ID, "loopSeq": input.Loop.Seq, "error": err.Error(),
			})
		}
		return nil, nil
	}
	if ask == nil {
		return nil, nil
	}
	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	return &awaitingHumanError{
		question:          ask.Question,
		options:           ask.Options,
		sessionID:         sessionID,
		executionID:       executionID,
		vendor:            vendor,
		recommendation:    ask.Recommendation,
		recommendedOption: ask.RecommendedOption,
		consequences:      ask.Consequences,
		confidence:        ask.Confidence,
	}, nil
}

func (r *Runner) latestAgentSession(ctx context.Context, loopID string) (string, string) {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return "", ""
	}
	rec, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || rec == nil {
		return "", ""
	}
	sessionID := ""
	if rec.NativeSessionID != nil {
		sessionID = strings.TrimSpace(*rec.NativeSessionID)
	}
	return sessionID, rec.Vendor
}

// suspendForHuman parks a worker run as awaiting_human: it persists the ask
// state on the loop, transitions the loop to awaiting_human, cancels the claimed
// queue item (so /respond can requeue it), ends the run as interrupted
// (resumable from the checkpoint), and sends the ask-card. Only reached when
// hitl.enabled is true.
func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint workerCheckpoint, awaiting *awaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	ask := loops.HITLAsk{
		Question:          awaiting.question,
		Options:           awaiting.options,
		SessionID:         awaiting.sessionID,
		ExecutionID:       awaiting.executionID,
		Vendor:            awaiting.vendor,
		Status:            "awaiting",
		AskedAt:           nowISO,
		Recommendation:    awaiting.recommendation,
		RecommendedOption: awaiting.recommendedOption,
		Consequences:      awaiting.consequences,
		Confidence:        awaiting.confidence,
	}
	// GitHub transport (default): post the question on a (draft) PR before parking,
	// so the ask metadata carries the PR + comment id the answer-poll lane needs.
	// Best-effort — the loop still parks awaiting_human if delivery fails.
	if r.hitlTransportGitHub() {
		if err := r.deliverAskToGitHub(ctx, input, checkpoint, awaiting, &ask); err != nil && r.logger != nil {
			r.logger.Warn("worker HITL github ask delivery failed; loop parked awaiting human without a PR comment", map[string]any{
				"loopId": input.Loop.ID, "error": err.Error(),
			})
		}
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		if meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask); werr == nil {
			updated.MetadataJSON = &meta
		}
		// The agent re-asked after reading the queued human messages, so they're
		// consumed — clear the inbox so they aren't re-injected on the next resume.
		if meta, werr := loops.ClearHumanInbox(updated.MetadataJSON); werr == nil {
			updated.MetadataJSON = &meta
		}
		updated.Status = "awaiting_human"
		updated.LastRunAt = stringPtr(nowISO)
		updated.NextRunAt = nil
	}); err != nil {
		return ProcessResult{}, err
	}
	reason := "worker suspended awaiting human decision"
	if _, err := r.repos.Queue.CancelByLoop(ctx, input.Loop.ID, nowISO, &reason); err != nil {
		return ProcessResult{}, err
	}
	summary := "Awaiting human decision: " + awaiting.question
	if _, err := r.completeRun(ctx, run, "interrupted", summary, "", checkpoint); err != nil {
		return ProcessResult{}, err
	}
	if !r.hitlTransportGitHub() && r.hitlNotify != nil {
		notif := HITLAskNotification{
			ProjectID:         input.Project.ID,
			LoopID:            input.Loop.ID,
			LoopSeq:           input.Loop.Seq,
			RunID:             run.ID,
			Repo:              derefString(input.Loop.Repo),
			Title:             awaiting.question,
			Question:          awaiting.question,
			Options:           awaiting.options,
			Recommendation:    awaiting.recommendation,
			RecommendedOption: awaiting.recommendedOption,
			Consequences:      awaiting.consequences,
			Confidence:        awaiting.confidence,
		}
		// Source + trigger come from the loop's work metadata (issue #, url, author).
		if w := checkpoint.Work; w != nil {
			notif.TriggerLogin = w.TriggerLogin
			switch {
			case w.PRNumber > 0:
				notif.SourceType = "GitHub PR"
				notif.SourceRef = "#" + strconv.FormatInt(w.PRNumber, 10)
			case w.IssueNumber > 0:
				notif.SourceType = "GitHub Issue"
				notif.SourceRef = "#" + strconv.FormatInt(w.IssueNumber, 10)
				notif.SourceURL = w.IssueURL
			}
		}
		if err := r.hitlNotify(ctx, notif); err != nil && r.logger != nil {
			// The loop is already parked in awaiting_human; if the human is never
			// notified they must find it via the dashboard / API. Surface loudly so an
			// unconfigured or failing notifier can't silently strand a run.
			r.logger.Warn("worker HITL ask notification failed; loop parked awaiting human with no notification sent", map[string]any{
				"loopId": input.Loop.ID, "loopSeq": input.Loop.Seq, "runId": run.ID, "error": err.Error(),
			})
		}
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}

// hitlTransportGitHub reports whether the GitHub PR-comment ask transport is
// active. GitHub is the default when hitl is enabled and no transport is set.
func (r *Runner) hitlTransportGitHub() bool {
	t := strings.TrimSpace(strings.ToLower(r.hitlAnswerTransport))
	return t == "" || t == "github"
}

func (r *Runner) hitlAwaitingLabel() string {
	if l := strings.TrimSpace(r.hitlGitHub.AwaitingLabel); l != "" {
		return l
	}
	return "looper:awaiting-human"
}

// deliverAskToGitHub ensures a (draft) PR exists for the loop, posts the agent's
// question as a marked PR comment, labels the PR so the answer-poll lane finds
// it, and records the PR + comment id on the ask. Best-effort; returns an error
// the caller logs while still parking the loop awaiting_human.
func (r *Runner) deliverAskToGitHub(ctx context.Context, input stepInput, checkpoint workerCheckpoint, awaiting *awaitingHumanError, ask *loops.HITLAsk) error {
	repo := derefString(input.Loop.Repo)
	if repo == "" && checkpoint.Work != nil {
		repo = checkpoint.Work.Repo
	}
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("hitl github: no repo for loop %s", input.Loop.ID)
	}
	cwd := input.Project.RepoPath

	prNumber := int64(0)
	if checkpoint.PullRequest != nil && checkpoint.PullRequest.Number > 0 {
		prNumber = checkpoint.PullRequest.Number
	} else if input.Loop.PRNumber != nil && *input.Loop.PRNumber > 0 {
		prNumber = *input.Loop.PRNumber
	}
	// On a later ask (e.g. a multi-turn second question) the loop/checkpoint may not
	// carry the PR that an earlier ask already opened for this branch — find and
	// reuse it instead of trying to open a duplicate (which gh rejects).
	if prNumber == 0 && checkpoint.Worktree != nil && strings.TrimSpace(checkpoint.Worktree.Branch) != "" {
		base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
		var aliases []string
		if checkpoint.Work != nil {
			if base == "" {
				base = strings.TrimSpace(checkpoint.Work.BaseBranch)
			}
			aliases = buildWorkerBranchAliases(*checkpoint.Work, input.Loop.ID)
		}
		aliases = append(aliases, checkpoint.Worktree.Branch)
		if base == "" {
			base = "main"
		}
		if existing, err := r.findOpenPullRequestForBranch(ctx, repo, aliases, base, cwd); err == nil && existing != nil {
			prNumber = existing.Number
		}
	}
	if prNumber == 0 {
		created, err := r.ensureDraftPRForAsk(ctx, input, checkpoint, repo, cwd)
		if err != nil {
			return err
		}
		prNumber = created
	}
	// Persist the PR onto the loop so later asks + the answer-poll resolve it fast.
	if prNumber > 0 && (input.Loop.PRNumber == nil || *input.Loop.PRNumber != prNumber) {
		_, _ = r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
			updated.PRNumber = int64Ptr(prNumber)
		})
	}
	if prNumber == 0 {
		return fmt.Errorf("hitl github: could not resolve a PR for loop %s", input.Loop.ID)
	}

	body := buildGitHubAskComment(input.Loop.Seq, awaiting.question, awaiting.options, r.hitlGitHub.MentionLogins)
	res, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd})
	if err != nil {
		return err
	}
	if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{r.hitlAwaitingLabel()}, CWD: cwd}); err != nil && r.logger != nil {
		r.logger.Warn("hitl github: failed to add awaiting-human label", map[string]any{"repo": repo, "pr": prNumber, "error": err.Error()})
	}

	ask.Transport = "github"
	ask.PRNumber = prNumber
	ask.AskCommentID = res.ID
	return nil
}

// ensureDraftPRForAsk pushes the loop's WIP branch and opens a draft PR to carry
// the question. Requires committed WIP on the branch; returns an error when there
// is nothing to open a PR from.
func (r *Runner) ensureDraftPRForAsk(ctx context.Context, input stepInput, checkpoint workerCheckpoint, repo, cwd string) (int64, error) {
	if checkpoint.Worktree == nil || strings.TrimSpace(checkpoint.Worktree.Branch) == "" {
		return 0, fmt.Errorf("hitl github: no worktree branch to open a draft PR for loop %s", input.Loop.ID)
	}
	base := strings.TrimSpace(checkpoint.Worktree.BaseBranch)
	title := ""
	if checkpoint.Work != nil {
		if base == "" {
			base = strings.TrimSpace(checkpoint.Work.BaseBranch)
		}
		title = strings.TrimSpace(checkpoint.Work.Title)
	}
	if base == "" {
		base = "main"
	}
	if title == "" {
		title = "Looper WIP — awaiting a human decision"
	}
	worktreeRoot, err := workerWorktreeRoot(input.Project)
	if err != nil {
		return 0, err
	}
	if err := r.git.Push(ctx, PushInput{RepoPath: cwd, WorktreeRoot: worktreeRoot, WorktreePath: checkpoint.Worktree.Path, Branch: checkpoint.Worktree.Branch, ProtectedBranches: compactStrings([]string{base})}); err != nil {
		return 0, err
	}
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{
		Repo:       repo,
		HeadBranch: checkpoint.Worktree.Branch,
		BaseBranch: base,
		Title:      title,
		Body:       "🚧 Draft opened by looper to ask a mid-run question — see the comment below. Not ready for review.",
		Draft:      true,
		CWD:        cwd,
	})
	if err != nil {
		return 0, err
	}
	return created.Number, nil
}

const hitlGitHubAskMarkerPrefix = "<!-- looper:hitl:ask v=1"

// buildGitHubAskComment renders the ask as a PR comment carrying a machine marker
// (so the poll lane finds it and never mistakes it for a human answer).
func buildGitHubAskComment(loopSeq int64, question string, options []string, mentionLogins []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s loop=%d -->\n", hitlGitHubAskMarkerPrefix, loopSeq)
	b.WriteString("🤔 **looper needs a decision to continue.**\n\n")
	b.WriteString(strings.TrimSpace(question))
	for _, o := range options {
		if o = strings.TrimSpace(o); o != "" {
			fmt.Fprintf(&b, "\n- %s", o)
		}
	}
	b.WriteString("\n\nReply to this comment with your choice — a letter, an option, or free-form guidance. I'll pick it up and continue on this PR.")
	if m := githubMentionLine(mentionLogins); m != "" {
		b.WriteString("\n\n" + m)
	}
	return b.String()
}

func githubMentionLine(logins []string) string {
	parts := make([]string, 0, len(logins))
	for _, l := range logins {
		if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "@")); l != "" {
			parts = append(parts, "@"+l)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "/cc " + strings.Join(parts, " ")
}
