package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

// hitlSentinelRelPath is where an agent writes a mid-run question, relative to
// the worktree root. Mirrors synclo's afk ask sentinel.
const hitlSentinelRelPath = ".looper/ask.json"

// hitlPromptInstruction is appended to the worker prompt ONLY when hitl.enabled
// is true. It tells the agent how to pause and ask a human instead of guessing.
const hitlPromptInstruction = "\n\n---\nHUMAN-IN-THE-LOOP: If you hit a point where you genuinely need a human decision to proceed — an ambiguous product/design choice, a risky or irreversible action, or missing information only a human has — do NOT guess. Instead write a JSON file at `.looper/ask.json` in the repository root with the shape {\"question\": \"<one concise question>\", \"options\": [\"<option 1>\", \"<option 2>\"]}, then STOP immediately without making further changes. A human will answer and you will be resumed in this same session with their decision. Use this only for genuine blockers; when a reasonable default exists, proceed autonomously.\n---"

type hitlAsk struct {
	Question string   `json:"question"`
	Options  []string `json:"options"`
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
	return &awaitingHumanError{question: ask.Question, options: ask.Options, sessionID: sessionID, executionID: executionID, vendor: vendor}, nil
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
		Question:    awaiting.question,
		Options:     awaiting.options,
		SessionID:   awaiting.sessionID,
		ExecutionID: awaiting.executionID,
		Vendor:      awaiting.vendor,
		Status:      "awaiting",
		AskedAt:     nowISO,
	}
	if _, err := r.updateLoop(ctx, input.Loop, func(updated *storage.LoopRecord) {
		if meta, werr := loops.WriteHITLAsk(updated.MetadataJSON, ask); werr == nil {
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
	if r.hitlNotify != nil {
		if err := r.hitlNotify(ctx, HITLAskNotification{
			ProjectID: input.Project.ID,
			LoopID:    input.Loop.ID,
			LoopSeq:   input.Loop.Seq,
			RunID:     run.ID,
			Repo:      derefString(input.Loop.Repo),
			Title:     awaiting.question,
			Question:  awaiting.question,
			Options:   awaiting.options,
		}); err != nil && r.logger != nil {
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
