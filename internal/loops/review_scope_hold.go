package loops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/storage"
)

const (
	// HITLKindReviewScopeHuman marks a scope-ambiguity ask, not a budget refill.
	// Continue resumes against current evidence without resetting role meters.
	HITLKindReviewScopeHuman = "review_scope_human"

	// ReviewScopeHumanRequiredReason is the durable no-HITL / exhausted-role
	// pause reason when Reviewer emits needs_human.
	ReviewScopeHumanRequiredReason = "review_scope_human_required"

	// ReviewScopeHumanSiblingPauseReason parks the opposite role while scope is held.
	ReviewScopeHumanSiblingPauseReason = "sibling_review_scope_human"

	reviewScopeHumanMetadataKey      = "reviewScopeHuman"
	reviewScopeHumanHandoffEventType = "loop.review_scope_human.required"
)

// ErrReviewScopeHumanInvalidAnswer is returned when a scope HITL answer is not
// Continue or Stop.
var ErrReviewScopeHumanInvalidAnswer = fmt.Errorf("review scope answer must be %q or %q", ReviewFixBudgetAnswerContinue, ReviewFixBudgetAnswerStop)

// ReviewScopeHumanState is durable pair-hold metadata for needs_human scope holds.
// It does not store role meters; Continue must not refill budgets.
//
// Pending marks deferred scope evidence while a budget hold is the sole primary
// hold (no status/ask change). After budget Continue releases the pair, pending
// is promoted to a real scope park.
type ReviewScopeHumanState struct {
	HeldBy         string `json:"heldBy,omitempty"`
	PauseReason    string `json:"pauseReason,omitempty"`
	HandoffEventAt string `json:"handoffEventAt,omitempty"`
	Question       string `json:"question,omitempty"`
	Evidence       string `json:"evidence,omitempty"`
	Pending        bool   `json:"pending,omitempty"`
	PendingHITL    bool   `json:"pendingHitl,omitempty"`
}

func NewReviewScopeHumanAsk(role, repo string, prNumber int64, question, nowISO string) HITLAsk {
	target := strings.TrimSpace(repo)
	if prNumber > 0 {
		target = fmt.Sprintf("%s#%d", target, prNumber)
	}
	q := strings.TrimSpace(question)
	if q == "" {
		q = fmt.Sprintf("%s needs human scope judgment on %s. Clarify the repository rule, PR goal/non-goal, or linked spec that must change before unpause. Continue resumes against current evidence, or stop the pair?", strings.TrimSpace(role), target)
	}
	return HITLAsk{
		Kind:     HITLKindReviewScopeHuman,
		Question: q,
		Options:  []string{ReviewFixBudgetAnswerContinue, ReviewFixBudgetAnswerStop},
		Status:   "awaiting",
		AskedAt:  nowISO,
		PRNumber: prNumber,
	}
}

func IsReviewScopeHumanAsk(ask HITLAsk) bool {
	return strings.TrimSpace(ask.Kind) == HITLKindReviewScopeHuman
}

func ReadReviewScopeHumanState(metadataJSON *string) ReviewScopeHumanState {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[reviewScopeHumanMetadataKey]
	if !ok {
		return ReviewScopeHumanState{}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ReviewScopeHumanState{}
	}
	var state ReviewScopeHumanState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return ReviewScopeHumanState{}
	}
	return state
}

func WriteReviewScopeHumanState(metadataJSON *string, state ReviewScopeHumanState) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	var asMap map[string]any
	if err := json.Unmarshal(encoded, &asMap); err != nil {
		return "", err
	}
	if len(asMap) == 0 {
		delete(meta, reviewScopeHumanMetadataKey)
	} else {
		meta[reviewScopeHumanMetadataKey] = asMap
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func IsSiblingReviewScopeHumanPause(metadataJSON *string) bool {
	if reason, _ := stringFromAny(parseMetadataObject(metadataJSON)["pauseReason"]); reason == ReviewScopeHumanSiblingPauseReason {
		return true
	}
	return ReadReviewScopeHumanState(metadataJSON).PauseReason == ReviewScopeHumanSiblingPauseReason
}

func IsReviewScopeHumanRequiredPause(metadataJSON *string) bool {
	if reason, _ := stringFromAny(parseMetadataObject(metadataJSON)["pauseReason"]); reason == ReviewScopeHumanRequiredReason {
		return true
	}
	return ReadReviewScopeHumanState(metadataJSON).PauseReason == ReviewScopeHumanRequiredReason
}

// IsReviewScopeHumanHold reports a needs_human pair hold (ask, no-ask pause, or sibling).
// Pure scope holds are not budget holds: Continue must not reset meters.
// Siblings in failed/interrupted/awaiting_human may carry scope hold metadata without
// flipping status to paused; HeldBy/pauseReason still gate independent resume.
// Pending-only evidence (deferred under a budget hold) is not an active scope hold.
func IsReviewScopeHumanHold(loop storage.LoopRecord) bool {
	switch strings.TrimSpace(loop.Status) {
	case "terminated", "stopped", "completed":
		return false
	}
	state := ReadReviewScopeHumanState(loop.MetadataJSON)
	if strings.TrimSpace(state.HeldBy) != "" {
		return true
	}
	if state.PauseReason == ReviewScopeHumanSiblingPauseReason || state.PauseReason == ReviewScopeHumanRequiredReason {
		return true
	}
	if IsSiblingReviewScopeHumanPause(loop.MetadataJSON) || IsReviewScopeHumanRequiredPause(loop.MetadataJSON) {
		return true
	}
	if strings.TrimSpace(loop.Status) == "awaiting_human" {
		ask, ok := ReadHITLAsk(loop.MetadataJSON)
		return ok && IsReviewScopeHumanAsk(ask)
	}
	return false
}

// HasPendingReviewScopeHuman reports deferred needs_human evidence waiting for
// budget release before a real scope park.
func HasPendingReviewScopeHuman(loop storage.LoopRecord) bool {
	state := ReadReviewScopeHumanState(loop.MetadataJSON)
	return state.Pending && strings.TrimSpace(state.HeldBy) == ""
}

// PersistPendingReviewScopeHumanEvidence stores needs_human question/evidence
// without changing status or writing a HITL ask. Used when budget is already the
// primary hold so the pair never stacks two asks/reasons.
func PersistPendingReviewScopeHumanEvidence(metadataJSON *string, question, evidence string, hitlEnabled bool) (string, error) {
	state := ReadReviewScopeHumanState(metadataJSON)
	if strings.TrimSpace(state.HeldBy) != "" {
		// Active scope hold already owns the pair; keep existing hold fields.
		if q := strings.TrimSpace(question); q != "" {
			state.Question = q
		}
		if e := strings.TrimSpace(evidence); e != "" {
			state.Evidence = e
		}
		return WriteReviewScopeHumanState(metadataJSON, state)
	}
	state.Pending = true
	state.PendingHITL = hitlEnabled
	if q := strings.TrimSpace(question); q != "" {
		state.Question = q
	}
	if e := strings.TrimSpace(evidence); e != "" {
		state.Evidence = e
	}
	// Pending must not look like an active sibling/required pause.
	if state.PauseReason == ReviewScopeHumanSiblingPauseReason || state.PauseReason == ReviewScopeHumanRequiredReason {
		state.PauseReason = ""
	}
	return WriteReviewScopeHumanState(metadataJSON, state)
}

// IsReviewFixPairHold is true for budget holds or scope-human holds.
func IsReviewFixPairHold(loop storage.LoopRecord) bool {
	return IsReviewFixBudgetHold(loop) || IsReviewScopeHumanHold(loop)
}

type ParkReviewScopeHumanInput struct {
	Held        storage.LoopRecord
	Role        string
	Repo        string
	PRNumber    int64
	NowISO      string
	HITLEnabled bool
	// Question is the exact scope question shown on HITL / handoff evidence.
	Question string
	// Evidence is optional structured handoff text (authority conflict, etc.).
	Evidence string
	DB       *sql.DB
}

// ParkReviewScopeHuman parks the held role and pauses same-lane siblings for a
// needs_human scope decision. Does not use budget Continue/Stop refill semantics.
func ParkReviewScopeHuman(ctx context.Context, repos *storage.Repositories, input ParkReviewScopeHumanInput) (storage.LoopRecord, error) {
	if repos == nil || repos.Loops == nil {
		return input.Held, fmt.Errorf("review scope park requires loop storage")
	}
	if input.DB != nil {
		var parked storage.LoopRecord
		err := storage.WithTransaction(ctx, input.DB, nil, func(tx *sql.Tx) error {
			var parkErr error
			parked, parkErr = parkReviewScopeHumanBody(ctx, storage.NewRepositories(tx), input)
			return parkErr
		})
		return parked, err
	}
	return parkReviewScopeHumanBody(ctx, repos, input)
}

func parkReviewScopeHumanBody(ctx context.Context, repos *storage.Repositories, input ParkReviewScopeHumanInput) (storage.LoopRecord, error) {
	held := input.Held
	if fresh, err := repos.Loops.GetByID(ctx, held.ID); err == nil && fresh != nil {
		held = *fresh
	}
	if isReviewScopeHumanPrimaryHold(held) {
		if err := finishReviewScopeHumanPark(ctx, repos, held, input.Role, input.NowISO); err != nil {
			return held, err
		}
		if err := ensureReviewScopeHumanHandoffEvent(ctx, repos, held, input); err != nil {
			return held, err
		}
		return held, nil
	}

	if err := cancelReviewScopeHumanQueues(ctx, repos, held, input.NowISO); err != nil {
		return input.Held, err
	}
	if err := parkSiblingReviewScopeHumanLoop(ctx, repos, held, input.Role, input.NowISO); err != nil {
		return input.Held, err
	}

	role := strings.TrimSpace(input.Role)
	state := ReadReviewScopeHumanState(held.MetadataJSON)
	state.HeldBy = role
	state.Question = strings.TrimSpace(input.Question)
	if e := strings.TrimSpace(input.Evidence); e != "" {
		state.Evidence = e
	}
	// Promoting from deferred budget path: clear pending markers.
	state.Pending = false
	state.PendingHITL = false
	state.PauseReason = ""
	if !input.HITLEnabled {
		state.PauseReason = ReviewScopeHumanRequiredReason
	}
	metadata, err := WriteReviewScopeHumanState(held.MetadataJSON, state)
	if err != nil {
		return input.Held, err
	}
	if input.HITLEnabled {
		ask := NewReviewScopeHumanAsk(input.Role, input.Repo, input.PRNumber, input.Question, input.NowISO)
		metadata, err = WriteHITLAsk(&metadata, ask)
		if err != nil {
			return input.Held, err
		}
		held.Status = "awaiting_human"
	} else {
		if ask, ok := ReadHITLAsk(&metadata); ok && IsReviewScopeHumanAsk(ask) {
			cleared, clearErr := ClearHITLAsk(&metadata)
			if clearErr != nil {
				return input.Held, clearErr
			}
			metadata = cleared
		}
		meta := parseMetadataObject(&metadata)
		meta["pauseReason"] = ReviewScopeHumanRequiredReason
		encoded, marshalErr := json.Marshal(meta)
		if marshalErr != nil {
			return input.Held, marshalErr
		}
		metadata = string(encoded)
		held.Status = "paused"
	}
	held.MetadataJSON = &metadata
	held.NextRunAt = nil
	held.UpdatedAt = input.NowISO
	if err := repos.Loops.Upsert(ctx, held); err != nil {
		return input.Held, err
	}
	if repos.Queue != nil {
		reason := ReviewScopeHumanRequiredReason
		if _, err := repos.Queue.CancelByLoop(ctx, held.ID, input.NowISO, &reason); err != nil {
			return held, err
		}
	}
	if err := ensureReviewScopeHumanHandoffEvent(ctx, repos, held, input); err != nil {
		return held, err
	}
	return held, nil
}

func isReviewScopeHumanPrimaryHold(loop storage.LoopRecord) bool {
	switch strings.TrimSpace(loop.Status) {
	case "awaiting_human":
		ask, ok := ReadHITLAsk(loop.MetadataJSON)
		return ok && IsReviewScopeHumanAsk(ask)
	case "paused":
		return IsReviewScopeHumanRequiredPause(loop.MetadataJSON) ||
			(strings.TrimSpace(ReadReviewScopeHumanState(loop.MetadataJSON).HeldBy) != "" && !IsSiblingReviewScopeHumanPause(loop.MetadataJSON))
	default:
		return false
	}
}

func finishReviewScopeHumanPark(ctx context.Context, repos *storage.Repositories, held storage.LoopRecord, heldBy, nowISO string) error {
	if err := cancelReviewScopeHumanQueues(ctx, repos, held, nowISO); err != nil {
		return err
	}
	return parkSiblingReviewScopeHumanLoop(ctx, repos, held, heldBy, nowISO)
}

func cancelReviewScopeHumanQueues(ctx context.Context, repos *storage.Repositories, held storage.LoopRecord, nowISO string) error {
	if repos.Queue == nil {
		return nil
	}
	reason := ReviewScopeHumanRequiredReason
	if _, err := repos.Queue.CancelByLoop(ctx, held.ID, nowISO, &reason); err != nil {
		return err
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	siblingReason := ReviewScopeHumanSiblingPauseReason
	for _, sibling := range FindSiblingReviewFixLoops(all, held) {
		if !isReviewScopeSiblingHoldApplicable(sibling.Status) {
			continue
		}
		if _, err := repos.Queue.CancelByLoop(ctx, sibling.ID, nowISO, &siblingReason); err != nil {
			return err
		}
	}
	return nil
}

func parkSiblingReviewScopeHumanLoop(ctx context.Context, repos *storage.Repositories, held storage.LoopRecord, heldBy, nowISO string) error {
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	for _, sibling := range FindSiblingReviewFixLoops(all, held) {
		if err := parkOneSiblingReviewScopeHumanLoop(ctx, repos, sibling, heldBy, nowISO); err != nil {
			return err
		}
	}
	return nil
}

// isReviewScopeSiblingHoldApplicable is true for every non-terminal opposite-role
// sibling. Unlike budget park, failed/interrupted/awaiting_human/independently
// paused siblings still receive scope hold metadata so independent resume is gated.
func isReviewScopeSiblingHoldApplicable(status string) bool {
	switch strings.TrimSpace(status) {
	case "terminated", "stopped", "completed":
		return false
	default:
		return true
	}
}

func parkOneSiblingReviewScopeHumanLoop(ctx context.Context, repos *storage.Repositories, sibling storage.LoopRecord, heldBy, nowISO string) error {
	if !isReviewScopeSiblingHoldApplicable(sibling.Status) {
		return nil
	}
	// Already carrying sibling scope hold: refresh queue cancel only.
	if IsSiblingReviewScopeHumanPause(sibling.MetadataJSON) && strings.TrimSpace(ReadReviewScopeHumanState(sibling.MetadataJSON).HeldBy) != "" {
		if repos.Queue != nil {
			reason := ReviewScopeHumanSiblingPauseReason
			if _, err := repos.Queue.CancelByLoop(ctx, sibling.ID, nowISO, &reason); err != nil {
				return err
			}
		}
		return nil
	}
	state := ReadReviewScopeHumanState(sibling.MetadataJSON)
	state.HeldBy = strings.TrimSpace(heldBy)
	state.PauseReason = ReviewScopeHumanSiblingPauseReason
	metadata, err := WriteReviewScopeHumanState(sibling.MetadataJSON, state)
	if err != nil {
		return err
	}
	updated := sibling
	updated.MetadataJSON = &metadata
	updated.UpdatedAt = nowISO
	// Preserve mid-run HITL asks and non-runnable terminal-ish statuses; only
	// flip actively schedulable statuses to paused with a top-level pauseReason.
	switch strings.TrimSpace(sibling.Status) {
	case "awaiting_human", "failed", "interrupted", "human_takeover":
		// Keep status and any existing HITL ask body; metadata gates resume.
		updated.NextRunAt = nil
	case "paused":
		// Keep paused. Scope overlay lives only in reviewScopeHuman metadata
		// (HeldBy / state.PauseReason). Never stamp top-level pauseReason —
		// manual pause has no reason and must not become scope-primary on Continue.
		updated.NextRunAt = nil
	default:
		// Schedulable → paused with scope sibling as the primary top-level reason.
		// Continue may requeue only when this top-level stamp is cleared.
		meta := parseMetadataObject(&metadata)
		meta["pauseReason"] = ReviewScopeHumanSiblingPauseReason
		encoded, marshalErr := json.Marshal(meta)
		if marshalErr != nil {
			return marshalErr
		}
		text := string(encoded)
		updated.MetadataJSON = &text
		updated.Status = "paused"
		updated.NextRunAt = nil
	}
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	if repos.Queue != nil {
		reason := ReviewScopeHumanSiblingPauseReason
		if _, err := repos.Queue.CancelByLoop(ctx, updated.ID, nowISO, &reason); err != nil {
			return err
		}
	}
	return nil
}

func ensureReviewScopeHumanHandoffEvent(ctx context.Context, repos *storage.Repositories, held storage.LoopRecord, input ParkReviewScopeHumanInput) error {
	if repos == nil || repos.Events == nil {
		return nil
	}
	state := ReadReviewScopeHumanState(held.MetadataJSON)
	if strings.TrimSpace(state.HandoffEventAt) != "" {
		return nil
	}
	if err := appendReviewScopeHumanHandoffEvent(ctx, repos, held, input); err != nil {
		return err
	}
	state = ReadReviewScopeHumanState(held.MetadataJSON)
	state.HandoffEventAt = input.NowISO
	encoded, err := WriteReviewScopeHumanState(held.MetadataJSON, state)
	if err != nil {
		return err
	}
	updated := held
	updated.MetadataJSON = &encoded
	updated.UpdatedAt = input.NowISO
	return repos.Loops.Upsert(ctx, updated)
}

func appendReviewScopeHumanHandoffEvent(ctx context.Context, repos *storage.Repositories, held storage.LoopRecord, input ParkReviewScopeHumanInput) error {
	if repos == nil || repos.Events == nil {
		return nil
	}
	lane := reviewFixBudgetLane(held)
	resume := reviewFixBudgetHandoffResume(held, input.HITLEnabled)
	projectID := strings.TrimSpace(held.ProjectID)
	loopID := strings.TrimSpace(held.ID)
	payload := map[string]any{
		"level":       "action_required",
		"kind":        HITLKindReviewScopeHuman,
		"repo":        strings.TrimSpace(input.Repo),
		"prNumber":    input.PRNumber,
		"lane":        lane,
		"heldBy":      strings.TrimSpace(input.Role),
		"reason":      ReviewScopeHumanRequiredReason,
		"hitlEnabled": input.HITLEnabled,
		"resume":      resume,
		"question":    strings.TrimSpace(input.Question),
	}
	if evidence := strings.TrimSpace(input.Evidence); evidence != "" {
		payload["evidence"] = evidence
	}
	if head := reviewFixBudgetHandoffHead(held); head != "" {
		payload["head"] = head
	}
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType:  reviewScopeHumanHandoffEventType,
		ProjectID:  optionalBudgetString(projectID),
		LoopID:     optionalBudgetString(loopID),
		EntityType: optionalBudgetString("loop"),
		EntityID:   optionalBudgetString(loopID),
		ActorType:  optionalBudgetString("system"),
		ActorID:    optionalBudgetString("review-scope-human"),
		Payload:    payload,
	})
}

type ReviewScopeHumanAnswerResult struct {
	Applied bool
	Action  string
	Loop    storage.LoopRecord
}

// ApplyReviewScopeHumanAnswer handles Continue (release pair, no meter reset) or
// Stop (terminate both). Returns Applied=false when not a scope hold.
func ApplyReviewScopeHumanAnswer(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, answer, nowISO string) (ReviewScopeHumanAnswerResult, error) {
	if !IsReviewScopeHumanHold(loop) {
		return ReviewScopeHumanAnswerResult{}, nil
	}
	if repos == nil || repos.Loops == nil {
		return ReviewScopeHumanAnswerResult{}, fmt.Errorf("review scope answer requires loop storage")
	}
	if IsReviewFixBudgetStop(answer) {
		updated, err := terminateReviewFixPair(ctx, repos, loop, nowISO, ReviewScopeHumanRequiredReason)
		if err != nil {
			return ReviewScopeHumanAnswerResult{}, err
		}
		return ReviewScopeHumanAnswerResult{Applied: true, Action: "stop", Loop: updated}, nil
	}
	if !IsReviewFixBudgetContinue(answer) {
		return ReviewScopeHumanAnswerResult{}, ErrReviewScopeHumanInvalidAnswer
	}
	updated, err := continueReviewScopeHuman(ctx, repos, loop, nowISO)
	if err != nil {
		return ReviewScopeHumanAnswerResult{}, err
	}
	return ReviewScopeHumanAnswerResult{Applied: true, Action: "continue", Loop: updated}, nil
}

func continueReviewScopeHuman(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return loop, err
	}
	members := reviewFixBudgetPairMembers(all, loop)

	// Release answered side first; do not touch role meters.
	if err := releaseOneReviewScopeHumanHold(ctx, repos, loop.ID, nowISO); err != nil {
		return loop, err
	}
	for _, member := range members {
		if member.ID == loop.ID {
			continue
		}
		if err := releaseOneReviewScopeHumanHold(ctx, repos, member.ID, nowISO); err != nil {
			return loop, err
		}
	}
	fresh, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return loop, err
	}
	if fresh == nil {
		return loop, fmt.Errorf("review scope continue lost loop %s", loop.ID)
	}
	return *fresh, nil
}

func releaseOneReviewScopeHumanHold(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return nil
	}
	if !IsReviewScopeHumanHold(*fresh) {
		return nil
	}
	priorStatus := strings.TrimSpace(fresh.Status)
	metadata := fresh.MetadataJSON
	if ask, ok := ReadHITLAsk(metadata); ok && IsReviewScopeHumanAsk(ask) {
		cleared, clearErr := ClearHITLAsk(metadata)
		if clearErr != nil {
			return clearErr
		}
		metadata = &cleared
	}
	state := ReadReviewScopeHumanState(metadata)
	state.HeldBy = ""
	state.Question = ""
	state.Evidence = ""
	state.Pending = false
	state.PendingHITL = false
	if state.PauseReason == ReviewScopeHumanSiblingPauseReason || state.PauseReason == ReviewScopeHumanRequiredReason {
		state.PauseReason = ""
	}
	// Clear handoff marker so a later park emits a fresh event.
	state.HandoffEventAt = ""
	encoded, err := WriteReviewScopeHumanState(metadata, state)
	if err != nil {
		return err
	}
	meta := parseMetadataObject(&encoded)
	// Queue only when scope was the primary top-level reason (park flipped a
	// schedulable loop to paused with sibling/required stamp). Manual pause and
	// independent reasons never receive that stamp.
	clearedScopeTopLevel := false
	if reason, _ := stringFromAny(meta["pauseReason"]); reason == ReviewScopeHumanSiblingPauseReason || reason == ReviewScopeHumanRequiredReason {
		delete(meta, "pauseReason")
		clearedScopeTopLevel = true
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	text := string(out)
	updated := *fresh
	updated.MetadataJSON = &text
	updated.UpdatedAt = nowISO

	// Preserve a non-scope mid-run HITL ask; do not force-queue over it.
	if ask, ok := ReadHITLAsk(&text); ok && strings.TrimSpace(ask.Status) == "awaiting" {
		updated.Status = "awaiting_human"
		updated.NextRunAt = nil
		return repos.Loops.Upsert(ctx, updated)
	}
	// human_takeover and non-runnable terminal-ish statuses stay put.
	switch priorStatus {
	case "human_takeover", "failed", "interrupted":
		updated.Status = priorStatus
		updated.NextRunAt = nil
		return repos.Loops.Upsert(ctx, updated)
	}

	// After clearing only the scope overlay, restore any independent lifecycle
	// that remains (budget hold, other pauseReason). Only queue when scope was
	// the primary reason the loop was non-runnable.
	updated.Status = priorStatus
	if IsReviewFixBudgetHold(updated) {
		if priorStatus != "paused" && priorStatus != "awaiting_human" {
			updated.Status = "paused"
		}
		updated.NextRunAt = nil
		return repos.Loops.Upsert(ctx, updated)
	}
	if reason, ok := stringFromAny(meta["pauseReason"]); ok && reason != "" {
		// Independent non-scope pause (budget, fixer_zero_progress, etc.).
		updated.Status = "paused"
		updated.NextRunAt = nil
		return repos.Loops.Upsert(ctx, updated)
	}
	// Already paused without a top-level scope stamp we just cleared: manual
	// pause (empty pauseReason) or overlay-only sibling — stay paused.
	if priorStatus == "paused" && !clearedScopeTopLevel {
		updated.Status = "paused"
		updated.NextRunAt = nil
		return repos.Loops.Upsert(ctx, updated)
	}

	updated.Status = "queued"
	updated.NextRunAt = &nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	return requeueReviewFixBudgetLoop(ctx, repos, updated.ID, nowISO)
}
