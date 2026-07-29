package fixer

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

const fixerHITLInstruction = `HUMAN-IN-THE-LOOP: This mechanism applies only to non-native comment fix items represented in review_thread_replies. Decide implementation details yourself. Use "needs_human" only when one of those review requests creates a genuine conflict with repository rules or the pull request's documented intent, depends on private product intent, or requires a high-stakes sign-off. Do not use it for reversible implementation choices. Never use "needs_human" in Forgejo repair_results; follow that contract's fixed, declined, or deferred actions.

When human direction is truly required, set that fix item's review_thread_replies action to "needs_human", put the concrete conflict and decision needed in "explanation", and STOP. Do not push, dismiss reviews, post replies, resolve threads, or otherwise mutate remote state. Looper will pause and resume you after an operator answers through its existing control plane.`

func fixerHITLPromptFor(fixItems []FixItem) string {
	for _, item := range fixItems {
		if item.Type == "comment" && item.Source != NativeReviewCommentSource &&
			strings.TrimSpace(item.ID) != "" && strings.TrimSpace(item.ThreadID) != "" {
			return fixerHITLInstruction
		}
	}
	return ""
}

type awaitingHumanError struct {
	question    string
	options     []string
	executionID string
	vendor      string
}

func (e *awaitingHumanError) Error() string { return "fixer paused awaiting human decision" }

func asAwaitingHumanError(err error) (*awaitingHumanError, bool) {
	var typed *awaitingHumanError
	if errors.As(err, &typed) {
		return typed, true
	}
	return nil, false
}

func humanDecisionFromReplies(replies []replyExplanationEntry, executionID, vendor string) *awaitingHumanError {
	questions := make([]string, 0)
	for _, reply := range replies {
		if normalizeReplyAction(reply.Action) == string(replyActionNeedsHuman) {
			questions = append(questions, strings.TrimSpace(reply.Explanation))
		}
	}
	if len(questions) == 0 {
		return nil
	}
	return &awaitingHumanError{
		question: strings.Join(questions, "\n"),
		options: []string{
			"Keep the repository rules and documented PR intent",
			"Follow the reviewer request",
		},
		executionID: executionID,
		vendor:      strings.TrimSpace(vendor),
	}
}

func validateNeedsHumanReplies(stdout, stderr string, fixItems []FixItem, accepted []replyExplanationEntry) error {
	payload := extractCompletionMarkerPayload(stdout + "\n" + stderr)
	if strings.TrimSpace(payload) == "" {
		return nil
	}
	var result struct {
		Replies []struct {
			FixItemID   string `json:"fixItemId"`
			ThreadID    string `json:"threadId"`
			Action      string `json:"action"`
			Explanation string `json:"explanation"`
		} `json:"review_thread_replies"`
	}
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		if strings.Contains(strings.ToLower(payload), `"`+string(replyActionNeedsHuman)+`"`) {
			return fmt.Errorf("invalid needs_human completion payload: %w", err)
		}
		return nil
	}
	items := make(map[string]FixItem, len(fixItems))
	for _, item := range fixItems {
		if item.Type == "comment" && item.Source != NativeReviewCommentSource && item.ID != "" {
			items[item.ID] = item
		}
	}
	acceptedCount := 0
	for _, reply := range accepted {
		if normalizeReplyAction(reply.Action) == string(replyActionNeedsHuman) {
			acceptedCount++
		}
	}
	rawCount := 0
	seen := map[string]struct{}{}
	for _, reply := range result.Replies {
		if normalizeReplyAction(reply.Action) != string(replyActionNeedsHuman) {
			continue
		}
		rawCount++
		item, ok := items[strings.TrimSpace(reply.FixItemID)]
		_, duplicate := seen[item.ID]
		if !ok || duplicate || strings.TrimSpace(reply.ThreadID) == "" ||
			strings.TrimSpace(reply.ThreadID) != item.ThreadID ||
			sanitizeReplyExplanation(reply.Explanation) == "" {
			return fmt.Errorf("invalid needs_human decision for fix item %q", reply.FixItemID)
		}
		seen[item.ID] = struct{}{}
	}
	if rawCount != acceptedCount {
		return fmt.Errorf("invalid needs_human decision set")
	}
	return nil
}

func (r *Runner) pendingHumanAnswer(ctx context.Context, loop *storage.LoopRecord, agentVendor string) (string, string) {
	ask, ok := r.readFreshHITLAsk(ctx, loop)
	if !ok || ask.Status != "answered" || strings.TrimSpace(ask.Answer) == "" {
		return "", ""
	}
	prompt := fmt.Sprintf("An operator answered the question you asked earlier (%q). Their decision: %s\nContinue the repair using this decision; do not ask the same question again.", ask.Question, ask.Answer)
	if strings.TrimSpace(ask.Vendor) != strings.TrimSpace(agentVendor) {
		return prompt + "\nThe configured agent vendor changed, so continue in a fresh session.", ""
	}
	return prompt, strings.TrimSpace(ask.SessionID)
}

func shouldResumeHITLRepair(metadataJSON *string, runStatus string, failedStep FixerStep, checkpoint fixerCheckpoint) bool {
	if runStatus != "interrupted" || failedStep != stepRepair {
		return false
	}
	ask, ok := loops.ReadHITLAsk(metadataJSON)
	if !ok || strings.TrimSpace(ask.Answer) == "" {
		return false
	}
	if ask.Status == "answered" {
		return true
	}
	return ask.Status == "consumed" && checkpoint.Repair != nil &&
		validateCompletedRepairCheckpoint(checkpoint.Repair) == nil
}

func hasDeliveredHITLAnswer(metadataJSON *string) bool {
	ask, ok := loops.ReadHITLAsk(metadataJSON)
	if !ok || strings.TrimSpace(ask.Answer) == "" {
		return false
	}
	return ask.Status == "answered" || ask.Status == "consumed"
}

func (r *Runner) readFreshHITLAsk(ctx context.Context, loop *storage.LoopRecord) (loops.HITLAsk, bool) {
	metadata := loop.MetadataJSON
	if r.repos != nil && r.repos.Loops != nil {
		if fresh, err := r.repos.Loops.GetByID(ctx, loop.ID); err == nil && fresh != nil {
			metadata = fresh.MetadataJSON
		}
	}
	return loops.ReadHITLAsk(metadata)
}

func (r *Runner) markHumanAnswerConsumed(ctx context.Context, loop *storage.LoopRecord) error {
	if r.repos == nil || r.repos.Loops == nil {
		return nil
	}
	fresh, err := r.repos.Loops.GetByID(ctx, loop.ID)
	if err != nil || fresh == nil {
		return err
	}
	ask, ok := loops.ReadHITLAsk(fresh.MetadataJSON)
	if !ok || ask.Status != "answered" {
		return nil
	}
	ask.Status = "consumed"
	metadata, err := loops.WriteHITLAsk(fresh.MetadataJSON, ask)
	if err != nil {
		return err
	}
	fresh.MetadataJSON = &metadata
	fresh.UpdatedAt = r.nowISO()
	if err := r.repos.Loops.Upsert(ctx, *fresh); err != nil {
		return err
	}
	loop.MetadataJSON = &metadata
	return nil
}

func (r *Runner) latestAgentSession(ctx context.Context, loopID string) (string, string) {
	if r.repos == nil || r.repos.AgentExecutions == nil {
		return "", ""
	}
	execution, err := r.repos.AgentExecutions.GetLatestByLoopID(ctx, loopID)
	if err != nil || execution == nil {
		return "", ""
	}
	sessionID := ""
	if execution.NativeSessionID != nil {
		sessionID = strings.TrimSpace(*execution.NativeSessionID)
	}
	return sessionID, strings.TrimSpace(execution.Vendor)
}

func (r *Runner) suspendForHuman(ctx context.Context, input stepInput, run storage.RunRecord, checkpoint fixerCheckpoint, awaiting *awaitingHumanError) (ProcessResult, error) {
	nowISO := r.nowISO()
	sessionID, vendor := r.latestAgentSession(ctx, input.Loop.ID)
	if vendor == "" {
		vendor = awaiting.vendor
	}
	ask := loops.HITLAsk{
		Question:    awaiting.question,
		Options:     awaiting.options,
		SessionID:   sessionID,
		ExecutionID: awaiting.executionID,
		Vendor:      vendor,
		Status:      "awaiting",
		AskedAt:     nowISO,
	}
	if checkpoint.Worktree != nil {
		dismissPath := filepath.Join(checkpoint.Worktree.Path, ".looper", "dismiss.json")
		if err := os.Remove(dismissPath); err != nil && !os.IsNotExist(err) {
			return ProcessResult{}, fmt.Errorf("clear pre-answer fixer dismissal intent: %w", err)
		}
	}
	reason := "fixer suspended awaiting human decision"
	summary := "Awaiting human decision: " + awaiting.question
	if r.db == nil {
		return ProcessResult{}, fmt.Errorf("fixer HITL atomic parking requires a database")
	}
	if err := storage.WithTransaction(ctx, r.db, nil, func(tx *sql.Tx) error {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, input.Loop.ID)
		if err != nil {
			return err
		}
		if loop == nil {
			return fmt.Errorf("loop not found while parking fixer HITL: %s", input.Loop.ID)
		}
		if loop.Status == "terminated" {
			return fmt.Errorf("cannot park terminated fixer loop: %s", input.Loop.ID)
		}
		metadata, err := loops.WriteHITLAsk(loop.MetadataJSON, ask)
		if err != nil {
			return err
		}
		loop.MetadataJSON = &metadata
		loop.Status = "awaiting_human"
		loop.LastRunAt = stringPtr(nowISO)
		loop.NextRunAt = nil
		loop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, *loop); err != nil {
			return err
		}
		if _, err := repos.Queue.CancelByLoop(ctx, input.Loop.ID, nowISO, &reason); err != nil {
			return err
		}
		updatedRun := run
		updatedRun.Status = "interrupted"
		updatedRun.Summary = stringPtr(summary)
		checkpointJSON := mustMarshalJSON(checkpoint)
		updatedRun.CheckpointJSON = &checkpointJSON
		updatedRun.EndedAt = stringPtr(nowISO)
		updatedRun.LastHeartbeatAt = stringPtr(nowISO)
		updatedRun.UpdatedAt = nowISO
		return repos.Runs.Upsert(ctx, updatedRun)
	}); err != nil {
		return ProcessResult{}, err
	}
	return ProcessResult{LoopID: input.Loop.ID, RunID: run.ID, QueueItemID: input.QueueItem.ID, Status: "awaiting_human", Summary: summary}, nil
}
