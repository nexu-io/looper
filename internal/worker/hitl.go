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
	loopcondition "github.com/nexu-io/looper/internal/loops/condition"
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
  "mentionOpenIds": ["<responsible member open_id, optional; invite and confirm membership first>"],
  "recommendation": "<1-2 sentences: what you found, what you'd do, and why>",
  "recommendedOption": "<the option you recommend, matching one of options>",
  "consequences": {"<option 1>": "<what happens if picked>", "<option 2>": "<what happens if picked>"},
  "confidence": "<high|medium|low>"
}
question + options are required; the rest are strongly encouraged (a bare question with no homework is a poor ask). Then STOP immediately without making further changes. A human will answer and you will be resumed in this same session with their decision.
---`

// Presentation and conversational judgment belong to the agent. The bridge
// only transports the existing ask fields and the next addressed reply.
const feishuCollaborationInstruction = `

FEISHU COLLABORATION:
- Before mentioning a selected person, invite them into the existing task group and verify they are a current member. An invitation or a pending approval is not membership. If they have not joined, tell the owner that the invitation is pending; do not claim the person has received an @mention. Continue independent work while waiting.
- Send collaboration messages and attachments ONLY as the bot, with an explicit --as bot when using lark-cli. Never use --as user to send on the owner's behalf unless the owner explicitly authorizes that specific message. Bot permission errors are not permission to impersonate the owner; report the missing permission instead. The host posts ask.json into the task thread, so do not send the same acceptance question yourself.
- The host delivers your updates and .looper/ask.json to the existing task thread and resumes this session with addressed human replies. Do not search Feishu tasks or chats to locate the task, and do not create a second task or group. Implement the assigned work first; use ask.json for acceptance or a required human decision.
- Write concise, plain Chinese for the people in the task thread. Lead with what changed or the specific question. Do not expose internal loop IDs, session IDs, local runtime paths, tool chatter, or generic progress slogans.
- Write final summaries as two or three short Markdown paragraphs or a few bullets: result, useful evidence, and any material remaining gap. Tool-by-tool narration belongs in the execution record, which the host folds; do not paste it into the answer. Preserve meaningful paragraph breaks and use links, code spans or a small table where helpful. A simple follow-up needs only a direct answer.
- Keep ask.json question to one or two short sentences: what needs judging and what to look at. Put member IDs only in mentionOpenIds, never in prose. Keep options short. Essential risks, costs and conditions for the decision must be visible in the question or option text; put supporting checks, reproduction steps, PR details and extended rationale in recommendation/consequences, which the card folds by default. Do not turn the question into a test report.
- The task anchor shows current progress; the thread holds the conversation. When asked a follow-up, answer it before repeating the remaining decision. Do not treat a question, disagreement, or "先不要执行" as approval. Messages labeled [群内讨论，仅作背景] are context, not instructions to proceed; respond to [群成员 @bot] and respect any conditions in that message.
- An @mention identifies the recipient, not the subject of the answer. In this task's acceptance thread, "验收通过" accepts the current deliverable unless the person explicitly limits its scope. Use the conversation context; do not ask them to confirm the same acceptance again. Continue the remaining PR work within existing authorization. Ask only about a genuinely new human decision; never invent merge authorization or repeat generic warnings about it in every message.
- Write options as plain, unnumbered strings in ask.json; the thread renders nonempty options as 1, 2, 3 in order. A reply may be a number, a request for explanation, a correction, or a delegated decision. Interpret it against the exact previous options. Do not invent approval buttons or require a rigid response phrase.
- Before asking for a judgment, prepare the smallest useful evidence: a working preview for behavior/UI, before-and-after screenshots for visual changes, a short recording for a sequence of steps, or a small comparison/table for tradeoffs. Explain what to look at and how it relates to the decision. Prefer one useful artifact over a pile of links. Use the tools actually available; never claim a screenshot, deployment, upload or check you did not perform.
- Put the key finding and real evidence links into question/recommendation, with each option's impact in consequences. The human cannot see your terminal output. A local path is not a shared link. Label localhost previews as available only on the host machine; state when an artifact cannot be shared. Do not invent URLs or image keys.
- Prefer native Feishu images/files over bare external attachment URLs. A URL that works on this machine is not proof the reviewer can open it. If a reviewer reports an inaccessible artifact, try an appropriate accessible alternative (native image, recording or file) and explain any interaction it cannot demonstrate; do not treat uploading a link as completed delivery.
- For delivery, state the user-visible result, how to view it, what you verified and the relevant remaining gap (for example: unit tests passed, browser flow not checked). A successful tool exit is not human acceptance, a merged PR, or a production release. Use a clear small table or annotated screenshot when it helps; do not force every answer into a long report.
`

type hitlAsk struct {
	Question          string            `json:"question"`
	MentionOpenIDs    []string          `json:"mentionOpenIds,omitempty"`
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
	question       string
	mentionOpenIDs []string
	options        []string
	sessionID      string
	executionID    string
	vendor         string
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
func (r *Runner) pendingTakeoverResume(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	tr, ok := loops.ReadTakeoverResume(loop.MetadataJSON)
	if !ok || strings.TrimSpace(tr.SessionID) == "" {
		return "", ""
	}
	prompt := strings.TrimSpace(tr.Prompt)
	if prompt == "" {
		prompt = "A human took this task's agent session over directly and has handed it back. Review the whole conversation so far — including their turns — and continue from where they left off; do not restart from scratch."
	}
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return takeoverCheckpointRestartPrompt(), ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loop.ID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(*execution.NativeSessionID) != strings.TrimSpace(tr.SessionID) || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return takeoverCheckpointRestartPrompt(), ""
	}
	return prompt, strings.TrimSpace(tr.SessionID)
}

func takeoverCheckpointRestartPrompt() string {
	return "A human took this task over directly and has handed it back. The configured agent vendor no longer matches the captured session, so start a fresh session, inspect the current worktree and task state, and continue without trying to attach to the old conversation."
}

// latestNativeSessionID returns the loop's most recent captured agent session id,
// so a mailbox-driven turn can native-resume the SAME session and have the full
// conversation context. Empty when none is recorded. agentVendor is the run's
// execution-authority vendor (snapshot when present).
func (r *Runner) latestNativeSessionID(ctx context.Context, loopID, agentVendor string) string {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil || execution.NativeSessionID == nil || strings.TrimSpace(execution.Vendor) != strings.TrimSpace(agentVendor) {
		return ""
	}
	return strings.TrimSpace(*execution.NativeSessionID)
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

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	if strings.TrimSpace(agentVendor) == "" {
		agentVendor = r.agentRuntime
	}
	resumePrompt := fmt.Sprintf("A human answered the question you asked earlier (%q). Their decision: %s\nContinue the task using this decision; do not ask the same question again.", ask.Question, ask.Answer)
	if ask.Transport == "feishu" {
		var previous strings.Builder
		number := 0
		for _, option := range ask.Options {
			if option = strings.TrimSpace(option); option != "" {
				number++
				fmt.Fprintf(&previous, "\n%d. %s", number, option)
				if consequence := strings.TrimSpace(ask.Consequences[option]); consequence != "" {
					fmt.Fprintf(&previous, " — %s", consequence)
				}
			}
		}
		resumePrompt = fmt.Sprintf("The responsible human replied in the task thread. Your previous question: %q\nPrevious options:%s\nPrevious recommendation: %s\nTheir message: %s\nRead this as conversation, not automatic approval. Answer follow-up questions in plain Chinese. If the decision is still unclear, explain and write .looper/ask.json again, then stop. The human sees only the question, options, recommendation and consequences from ask.json, so include your explanation and evidence there. Proceed with the blocked choice only when their addressed reply gives a clear decision or explicitly delegates it to you. A concrete requested correction authorizes that scoped revision: implement and verify it, then ask again for any remaining acceptance decision. Continue from the existing task and worktree; do not restart unrelated work.", ask.Question, previous.String(), ask.Recommendation, ask.Answer)
	}
	if ask.Transport == "feishu" {
		resumePrompt += feishuCollaborationInstruction
	}
	if strings.TrimSpace(ask.Vendor) != strings.TrimSpace(agentVendor) {
		return resumePrompt + "\nThe configured agent vendor changed after the question was asked, so continue in a fresh session rather than trying to attach to the prior vendor's session.", ""
	}
	return resumePrompt, strings.TrimSpace(ask.SessionID)
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

// detectHITLAskSentinel consumes the agent's source-of-truth ask sentinel and, when present,
// returns a typed awaitingHumanError carrying the question, options, and the
// agent's native session id (so the run can resume the same session).
func (r *Runner) detectHITLAskSentinel(ctx context.Context, input stepInput, worktreePath, executionID string) (*awaitingHumanError, error) {
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
		mentionOpenIDs:    ask.MentionOpenIDs,
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
		MentionOpenIDs:    awaiting.mentionOpenIDs,
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
	if strings.EqualFold(strings.TrimSpace(r.hitlAnswerTransport), "feishu") {
		ask.Transport = "feishu"
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
		if meta, werr := loopcondition.Set(updated.MetadataJSON, loopcondition.Record{Kind: loopcondition.HumanAnswered, Since: nowISO}); werr == nil {
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
	if r.hitlNotify != nil && (strings.TrimSpace(ask.ActionURL) != "" || ask.Transport == "feishu") {
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
		// Source + trigger come from the durable source-of-truth ask and work metadata.
		notif.SourceURL = ask.ActionURL
		if w := checkpoint.Work; w != nil {
			notif.TriggerLogin = w.TriggerLogin
			switch {
			case w.PRNumber > 0:
				notif.SourceType = "GitHub decision comment"
				notif.SourceRef = "#" + strconv.FormatInt(w.PRNumber, 10)
			case w.IssueNumber > 0:
				notif.SourceType = "GitHub Issue"
				notif.SourceRef = "#" + strconv.FormatInt(w.IssueNumber, 10)
			}
		}
		if ask.PRNumber > 0 {
			notif.SourceType = "GitHub decision comment"
			notif.SourceRef = "PR #" + strconv.FormatInt(ask.PRNumber, 10)
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
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	res, err := r.github.CreateIssueComment(ctx, IssueCommentInput{Repo: repo, IssueNumber: prNumber, Body: body, CWD: cwd, DisclosureAgent: disclosureAgent, DisclosureModel: disclosureModel})
	if err != nil {
		return err
	}
	if err := r.github.AddPullRequestLabels(ctx, PullRequestLabelsInput{Repo: repo, PRNumber: prNumber, Labels: []string{r.hitlAwaitingLabel()}, CWD: cwd}); err != nil && r.logger != nil {
		r.logger.Warn("hitl github: failed to add awaiting-human label", map[string]any{"repo": repo, "pr": prNumber, "error": err.Error()})
	}

	ask.Transport = "github"
	ask.ActionURL = strings.TrimSpace(res.URL)
	if ask.ActionURL == "" && res.ID > 0 {
		ask.ActionURL = fmt.Sprintf("https://github.com/%s/pull/%d#issuecomment-%d", strings.Trim(repo, "/"), prNumber, res.ID)
	}
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
	disclosureAgent, disclosureModel := r.disclosureIdentity(input.Run)
	created, err := r.github.CreatePullRequest(ctx, CreatePullRequestInput{
		Repo:            repo,
		HeadBranch:      checkpoint.Worktree.Branch,
		BaseBranch:      base,
		Title:           title,
		Body:            "🚧 Draft opened by looper to ask a mid-run question — see the comment below. Not ready for review.",
		Draft:           true,
		CWD:             cwd,
		DisclosureAgent: disclosureAgent,
		DisclosureModel: disclosureModel,
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
