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
	// HITLKindReviewFixBudget marks an ask that is a cycle-cap decision, not a
	// mid-run agent question. Resume must not replay an agent session.
	HITLKindReviewFixBudget = "review_fix_budget"

	ReviewFixBudgetAnswerContinue = "Continue"
	ReviewFixBudgetAnswerStop     = "Stop"

	ReviewFixBudgetPauseReason       = "sibling_review_fix_budget"
	ReviewFixBudgetTerminationReason = "review_fix_budget_exhausted"

	reviewFixBudgetMetadataKey = "reviewFixBudget"
	reviewerLoopMetadataKey    = "loop"
	reviewerIterationCountKey  = "iterationCount"

	reviewFixBudgetLaneAutomatic        = "automatic"
	reviewFixBudgetLaneContinuousManual = "continuous_manual"
	reviewFixBudgetHandoffEventType     = "loop.review_fix_budget.exhausted"
)

// ErrReviewFixBudgetInvalidAnswer is the only ApplyReviewFixBudgetAnswer
// failure that API clients should treat as a 400 validation error.
var ErrReviewFixBudgetInvalidAnswer = fmt.Errorf("review-fix budget answer must be %q or %q", ReviewFixBudgetAnswerContinue, ReviewFixBudgetAnswerStop)

// ReviewFixBudgetState is the durable PR-scoped ledger for the new caps.
// Authority for the cap is live config; this record only stores counts and
// which role exhausted.
type ReviewFixBudgetState struct {
	PushCount      int    `json:"pushCount,omitempty"`
	ExhaustedBy    string `json:"exhaustedBy,omitempty"`
	SiblingOf      string `json:"siblingOf,omitempty"`
	PauseReason    string `json:"pauseReason,omitempty"`
	HandoffEventAt string `json:"handoffEventAt,omitempty"`
}

// ReviewFixBudgetLiveCaps carries live role caps for Continue meter refill.
// Authority is current project/role config, not stored hold state.
type ReviewFixBudgetLiveCaps struct {
	ReviewerMaxPublishes int
	FixerMaxPushes       int
}

func BudgetExhausted(count, cap int) bool {
	return cap > 0 && count >= cap
}

func NewReviewFixBudgetAsk(role, repo string, prNumber int64, count, cap int, nowISO string) HITLAsk {
	target := strings.TrimSpace(repo)
	if prNumber > 0 {
		target = fmt.Sprintf("%s#%d", target, prNumber)
	}
	return HITLAsk{
		Kind:     HITLKindReviewFixBudget,
		Question: fmt.Sprintf("%s hit its review-fix budget on %s (%d/%d). Continue another %d, or stop review-fix on this PR?", strings.TrimSpace(role), target, count, cap, cap),
		Options:  []string{ReviewFixBudgetAnswerContinue, ReviewFixBudgetAnswerStop},
		Status:   "awaiting",
		AskedAt:  nowISO,
		PRNumber: prNumber,
	}
}

func IsReviewFixBudgetAsk(ask HITLAsk) bool {
	return strings.TrimSpace(ask.Kind) == HITLKindReviewFixBudget
}

func IsReviewFixBudgetContinue(answer string) bool {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	return normalized == strings.ToLower(ReviewFixBudgetAnswerContinue) || normalized == "continue another"
}

func IsReviewFixBudgetStop(answer string) bool {
	normalized := strings.ToLower(strings.TrimSpace(answer))
	return normalized == strings.ToLower(ReviewFixBudgetAnswerStop) || normalized == "stop"
}

func ReadReviewFixBudgetState(metadataJSON *string) ReviewFixBudgetState {
	meta := parseMetadataObject(metadataJSON)
	raw, ok := meta[reviewFixBudgetMetadataKey]
	if !ok {
		return ReviewFixBudgetState{}
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ReviewFixBudgetState{}
	}
	var state ReviewFixBudgetState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return ReviewFixBudgetState{}
	}
	return state
}

func WriteReviewFixBudgetState(metadataJSON *string, state ReviewFixBudgetState) (string, error) {
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
		delete(meta, reviewFixBudgetMetadataKey)
	} else {
		meta[reviewFixBudgetMetadataKey] = asMap
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func ReviewerPublishCount(metadataJSON *string) int {
	meta := parseMetadataObject(metadataJSON)
	loopMeta, _ := meta[reviewerLoopMetadataKey].(map[string]any)
	if loopMeta == nil {
		return 0
	}
	return intFromMetadata(loopMeta[reviewerIterationCountKey])
}

func IncrementReviewerPublishCount(metadataJSON *string) (string, int, error) {
	meta := parseMetadataObject(metadataJSON)
	loopMeta, _ := meta[reviewerLoopMetadataKey].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	count := intFromMetadata(loopMeta[reviewerIterationCountKey]) + 1
	loopMeta[reviewerIterationCountKey] = count
	meta[reviewerLoopMetadataKey] = loopMeta
	out, err := json.Marshal(meta)
	if err != nil {
		return "", 0, err
	}
	return string(out), count, nil
}

func ResetReviewerPublishCount(metadataJSON *string) (string, error) {
	meta := parseMetadataObject(metadataJSON)
	loopMeta, _ := meta[reviewerLoopMetadataKey].(map[string]any)
	if loopMeta == nil {
		loopMeta = map[string]any{}
	}
	loopMeta[reviewerIterationCountKey] = 0
	meta[reviewerLoopMetadataKey] = loopMeta
	out, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func IncrementFixerPushCount(metadataJSON *string) (string, int, error) {
	state := ReadReviewFixBudgetState(metadataJSON)
	state.PushCount++
	encoded, err := WriteReviewFixBudgetState(metadataJSON, state)
	if err != nil {
		return "", 0, err
	}
	return encoded, state.PushCount, nil
}

func ResetFixerPushCount(metadataJSON *string) (string, error) {
	state := ReadReviewFixBudgetState(metadataJSON)
	state.PushCount = 0
	state.ExhaustedBy = ""
	return WriteReviewFixBudgetState(metadataJSON, state)
}

func FixerPushCount(metadataJSON *string) int {
	return ReadReviewFixBudgetState(metadataJSON).PushCount
}

func IsSiblingReviewFixBudgetPause(metadataJSON *string) bool {
	if reason, _ := stringFromAny(parseMetadataObject(metadataJSON)["pauseReason"]); reason == ReviewFixBudgetPauseReason {
		return true
	}
	return ReadReviewFixBudgetState(metadataJSON).PauseReason == ReviewFixBudgetPauseReason
}

// IsReviewFixBudgetExhaustedPause reports a no-HITL exhausted-role hold.
func IsReviewFixBudgetExhaustedPause(metadataJSON *string) bool {
	if reason, _ := stringFromAny(parseMetadataObject(metadataJSON)["pauseReason"]); reason == ReviewFixBudgetTerminationReason {
		return true
	}
	state := ReadReviewFixBudgetState(metadataJSON)
	return state.PauseReason == ReviewFixBudgetTerminationReason
}

// IsReviewFixBudgetHold reports whether a loop is part of a review-fix budget
// hold (HITL ask, exhausted no-ask pause, or sibling paired pause).
func IsReviewFixBudgetHold(loop storage.LoopRecord) bool {
	switch strings.TrimSpace(loop.Status) {
	case "awaiting_human":
		ask, ok := ReadHITLAsk(loop.MetadataJSON)
		return ok && IsReviewFixBudgetAsk(ask)
	case "paused":
		if IsSiblingReviewFixBudgetPause(loop.MetadataJSON) || IsReviewFixBudgetExhaustedPause(loop.MetadataJSON) {
			return true
		}
		return strings.TrimSpace(ReadReviewFixBudgetState(loop.MetadataJSON).ExhaustedBy) != ""
	default:
		return false
	}
}

// ParticipatesInReviewFixBudget is true for automatic loops and continuous
// manual/takeover loops (manual + followUpdates). One-shot manual is exempt.
func ParticipatesInReviewFixBudget(loop storage.LoopRecord) bool {
	return reviewFixBudgetLane(loop) != ""
}

func reviewFixBudgetLane(loop storage.LoopRecord) string {
	meta := parseMetadataObject(loop.MetadataJSON)
	manual, _ := meta["manual"].(bool)
	followUpdates, _ := meta["followUpdates"].(bool)
	if !manual {
		return reviewFixBudgetLaneAutomatic
	}
	if followUpdates {
		return reviewFixBudgetLaneContinuousManual
	}
	return ""
}

func isTerminalReviewFixSiblingStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "terminated", "stopped", "completed", "awaiting_human", "human_takeover":
		return true
	default:
		return false
	}
}

func isReviewFixBudgetPauseApplicable(status string) bool {
	switch strings.TrimSpace(status) {
	case "failed", "interrupted":
		return false
	default:
		return !isTerminalReviewFixSiblingStatus(status)
	}
}

// FindSiblingReviewFixLoops returns opposite-role loops on the same
// project/repo/PR in the same derived lane (automatic or continuous_manual).
// One-shot manual loops are excluded. Automatic never pairs with continuous_manual.
func FindSiblingReviewFixLoops(all []storage.LoopRecord, loop storage.LoopRecord) []storage.LoopRecord {
	lane := reviewFixBudgetLane(loop)
	if lane == "" {
		return nil
	}
	wantType := ""
	switch strings.TrimSpace(loop.Type) {
	case "reviewer":
		wantType = "fixer"
	case "fixer":
		wantType = "reviewer"
	default:
		return nil
	}
	repo := derefLoopString(loop.Repo)
	pr := derefLoopInt64(loop.PRNumber)
	if repo == "" || pr == 0 {
		return nil
	}
	siblings := make([]storage.LoopRecord, 0)
	for _, candidate := range all {
		if candidate.ID == loop.ID || candidate.Type != wantType || candidate.ProjectID != loop.ProjectID {
			continue
		}
		if derefLoopString(candidate.Repo) != repo || derefLoopInt64(candidate.PRNumber) != pr {
			continue
		}
		if reviewFixBudgetLane(candidate) != lane {
			continue
		}
		siblings = append(siblings, candidate)
	}
	return siblings
}

type ParkReviewFixBudgetInput struct {
	Exhausted   storage.LoopRecord
	Role        string
	Repo        string
	PRNumber    int64
	Count       int
	Cap         int
	NowISO      string
	HITLEnabled bool
	// LiveCaps optional; used with pair metadata to snapshot both role meters
	// on the handoff event. Zero caps mean unknown for that role.
	LiveCaps ReviewFixBudgetLiveCaps
	// DB optional. When set, exhausted persist + sibling park + queue cancel
	// run in one transaction so a partial park cannot leave one role runnable.
	DB *sql.DB
}

// ParkReviewFixBudget parks the exhausted loop and pauses same-lane siblings.
// When HITLEnabled is true the exhausted role is awaiting_human with a Continue/Stop
// ask; when false it is paused with reason review_fix_budget_exhausted and no ask.
// Queue items on both sides are cancelled so discovery cannot immediately restart.
// Re-entry on an already-held budget loop finishes cancel/sibling park and retries
// a missing handoff event without rewriting a delivered ask or no-ask hold metadata.
func ParkReviewFixBudget(ctx context.Context, repos *storage.Repositories, input ParkReviewFixBudgetInput) (storage.LoopRecord, error) {
	if repos == nil || repos.Loops == nil {
		return input.Exhausted, fmt.Errorf("review-fix budget park requires loop storage")
	}
	if input.DB != nil {
		var parked storage.LoopRecord
		err := storage.WithTransaction(ctx, input.DB, nil, func(tx *sql.Tx) error {
			var parkErr error
			parked, parkErr = parkReviewFixBudgetBody(ctx, storage.NewRepositories(tx), input)
			return parkErr
		})
		return parked, err
	}
	return parkReviewFixBudgetBody(ctx, repos, input)
}

func parkReviewFixBudgetBody(ctx context.Context, repos *storage.Repositories, input ParkReviewFixBudgetInput) (storage.LoopRecord, error) {
	exhausted := input.Exhausted
	if fresh, err := repos.Loops.GetByID(ctx, exhausted.ID); err == nil && fresh != nil {
		exhausted = *fresh
	}
	if isReviewFixBudgetExhaustedHold(exhausted) {
		if err := finishReviewFixBudgetPark(ctx, repos, exhausted, input.Role, input.NowISO); err != nil {
			return exhausted, err
		}
		// Re-entry: retry a missing handoff event after the pair is non-runnable.
		if err := ensureReviewFixBudgetHandoffEvent(ctx, repos, exhausted, input); err != nil {
			return exhausted, err
		}
		return exhausted, nil
	}

	// Fail-closed order without relying on outer TX alone: cancel queues and
	// park siblings before persisting the exhausted hold so a sibling failure
	// never leaves the sibling runnable while the exhausted side is already held.
	if err := cancelReviewFixBudgetQueues(ctx, repos, exhausted, input.Role, input.NowISO); err != nil {
		return input.Exhausted, err
	}
	if err := parkSiblingReviewFixLoop(ctx, repos, exhausted, input.Role, input.NowISO); err != nil {
		return input.Exhausted, err
	}

	role := strings.TrimSpace(input.Role)
	state := ReadReviewFixBudgetState(exhausted.MetadataJSON)
	state.ExhaustedBy = role
	state.PauseReason = ""
	if !input.HITLEnabled {
		state.PauseReason = ReviewFixBudgetTerminationReason
	}
	metadata, err := WriteReviewFixBudgetState(exhausted.MetadataJSON, state)
	if err != nil {
		return input.Exhausted, err
	}
	if input.HITLEnabled {
		ask := NewReviewFixBudgetAsk(input.Role, input.Repo, input.PRNumber, input.Count, input.Cap, input.NowISO)
		metadata, err = WriteHITLAsk(&metadata, ask)
		if err != nil {
			return input.Exhausted, err
		}
		exhausted.Status = "awaiting_human"
	} else {
		if ask, ok := ReadHITLAsk(&metadata); ok && IsReviewFixBudgetAsk(ask) {
			cleared, clearErr := ClearHITLAsk(&metadata)
			if clearErr != nil {
				return input.Exhausted, clearErr
			}
			metadata = cleared
		}
		meta := parseMetadataObject(&metadata)
		meta["pauseReason"] = ReviewFixBudgetTerminationReason
		encoded, marshalErr := json.Marshal(meta)
		if marshalErr != nil {
			return input.Exhausted, marshalErr
		}
		metadata = string(encoded)
		exhausted.Status = "paused"
	}
	exhausted.MetadataJSON = &metadata
	exhausted.NextRunAt = nil
	exhausted.UpdatedAt = input.NowISO
	if err := repos.Loops.Upsert(ctx, exhausted); err != nil {
		return input.Exhausted, err
	}
	// Cancel exhausted queue again after status flip (idempotent).
	if repos.Queue != nil {
		reason := ReviewFixBudgetTerminationReason
		if _, err := repos.Queue.CancelByLoop(ctx, exhausted.ID, input.NowISO, &reason); err != nil {
			return exhausted, err
		}
	}
	if err := ensureReviewFixBudgetHandoffEvent(ctx, repos, exhausted, input); err != nil {
		// Pair is already non-runnable; surface the error so re-entry retries.
		return exhausted, err
	}
	return exhausted, nil
}

func isReviewFixBudgetExhaustedHold(loop storage.LoopRecord) bool {
	switch strings.TrimSpace(loop.Status) {
	case "awaiting_human":
		ask, ok := ReadHITLAsk(loop.MetadataJSON)
		return ok && IsReviewFixBudgetAsk(ask)
	case "paused":
		return IsReviewFixBudgetExhaustedPause(loop.MetadataJSON) ||
			(strings.TrimSpace(ReadReviewFixBudgetState(loop.MetadataJSON).ExhaustedBy) != "" && !IsSiblingReviewFixBudgetPause(loop.MetadataJSON))
	default:
		return false
	}
}

func finishReviewFixBudgetPark(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, exhaustedBy, nowISO string) error {
	if err := cancelReviewFixBudgetQueues(ctx, repos, exhausted, exhaustedBy, nowISO); err != nil {
		return err
	}
	return parkSiblingReviewFixLoop(ctx, repos, exhausted, exhaustedBy, nowISO)
}

func cancelReviewFixBudgetQueues(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, exhaustedBy, nowISO string) error {
	if repos.Queue == nil {
		return nil
	}
	reason := ReviewFixBudgetTerminationReason
	if _, err := repos.Queue.CancelByLoop(ctx, exhausted.ID, nowISO, &reason); err != nil {
		return err
	}
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	siblingReason := ReviewFixBudgetPauseReason
	for _, sibling := range FindSiblingReviewFixLoops(all, exhausted) {
		if !isReviewFixBudgetPauseApplicable(sibling.Status) {
			continue
		}
		if sibling.Status == "paused" && !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) && !IsReviewFixBudgetExhaustedPause(sibling.MetadataJSON) {
			continue
		}
		if _, err := repos.Queue.CancelByLoop(ctx, sibling.ID, nowISO, &siblingReason); err != nil {
			return err
		}
	}
	_ = exhaustedBy
	return nil
}

func parkSiblingReviewFixLoop(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, exhaustedBy, nowISO string) error {
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	for _, sibling := range FindSiblingReviewFixLoops(all, exhausted) {
		if err := parkOneSiblingReviewFixLoop(ctx, repos, sibling, exhaustedBy, nowISO); err != nil {
			return err
		}
	}
	return nil
}

// reviewFixBudgetSiblingParkHook, when set (tests only), runs before sibling
// persist so failure-path coverage can inject list/upsert errors.
var reviewFixBudgetSiblingParkHook func(sibling storage.LoopRecord) error

// reviewFixBudgetReleaseHook, when set (tests only), runs after a successful
// release of one hold so Continue failure after partial release can be injected.
var reviewFixBudgetReleaseHook func(loopID string) error

func parkOneSiblingReviewFixLoop(ctx context.Context, repos *storage.Repositories, sibling storage.LoopRecord, exhaustedBy, nowISO string) error {
	if reviewFixBudgetSiblingParkHook != nil {
		if err := reviewFixBudgetSiblingParkHook(sibling); err != nil {
			return err
		}
	}
	if !isReviewFixBudgetPauseApplicable(sibling.Status) {
		return nil
	}
	if sibling.Status == "paused" && !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) && !IsReviewFixBudgetExhaustedPause(sibling.MetadataJSON) {
		return nil
	}
	// Already sibling-paused: still cancel queue on re-entry.
	if sibling.Status == "paused" && IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
		if repos.Queue != nil {
			reason := ReviewFixBudgetPauseReason
			if _, err := repos.Queue.CancelByLoop(ctx, sibling.ID, nowISO, &reason); err != nil {
				return err
			}
		}
		return nil
	}
	state := ReadReviewFixBudgetState(sibling.MetadataJSON)
	state.SiblingOf = strings.TrimSpace(exhaustedBy)
	state.PauseReason = ReviewFixBudgetPauseReason
	metadata, err := WriteReviewFixBudgetState(sibling.MetadataJSON, state)
	if err != nil {
		return err
	}
	meta := parseMetadataObject(&metadata)
	meta["pauseReason"] = ReviewFixBudgetPauseReason
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	text := string(encoded)
	updated := sibling
	updated.MetadataJSON = &text
	updated.Status = "paused"
	updated.NextRunAt = nil
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	if repos.Queue != nil {
		reason := ReviewFixBudgetPauseReason
		if _, err := repos.Queue.CancelByLoop(ctx, updated.ID, nowISO, &reason); err != nil {
			return err
		}
	}
	return nil
}

func ensureReviewFixBudgetHandoffEvent(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, input ParkReviewFixBudgetInput) error {
	if repos == nil || repos.Events == nil {
		return nil
	}
	state := ReadReviewFixBudgetState(exhausted.MetadataJSON)
	if strings.TrimSpace(state.HandoffEventAt) != "" {
		return nil
	}
	if err := appendReviewFixBudgetHandoffEvent(ctx, repos, exhausted, input); err != nil {
		return err
	}
	state = ReadReviewFixBudgetState(exhausted.MetadataJSON)
	state.HandoffEventAt = input.NowISO
	encoded, err := WriteReviewFixBudgetState(exhausted.MetadataJSON, state)
	if err != nil {
		return err
	}
	updated := exhausted
	updated.MetadataJSON = &encoded
	updated.UpdatedAt = input.NowISO
	return repos.Loops.Upsert(ctx, updated)
}

func appendReviewFixBudgetHandoffEvent(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, input ParkReviewFixBudgetInput) error {
	if repos == nil || repos.Events == nil {
		return nil
	}
	lane := reviewFixBudgetLane(exhausted)
	resume := reviewFixBudgetHandoffResume(exhausted, input.HITLEnabled)
	head := reviewFixBudgetHandoffHead(exhausted)
	reviewerCount, reviewerCap, fixerCount, fixerCap, exhaustedRoles := reviewFixBudgetHandoffMeters(ctx, repos, exhausted, input)
	projectID := strings.TrimSpace(exhausted.ProjectID)
	loopID := strings.TrimSpace(exhausted.ID)
	exhaustedBy := strings.TrimSpace(input.Role)
	if len(exhaustedRoles) > 1 {
		exhaustedBy = strings.Join(exhaustedRoles, ",")
	}
	payload := map[string]any{
		"level":          "action_required",
		"kind":           HITLKindReviewFixBudget,
		"repo":           strings.TrimSpace(input.Repo),
		"prNumber":       input.PRNumber,
		"lane":           lane,
		"exhaustedBy":    exhaustedBy,
		"exhaustedRoles": exhaustedRoles,
		"hitlEnabled":    input.HITLEnabled,
		"resume":         resume,
		"reviewer":       map[string]any{"count": reviewerCount, "cap": reviewerCap},
		"fixer":          map[string]any{"count": fixerCount, "cap": fixerCap},
	}
	if head != "" {
		payload["head"] = head
	}
	return eventlog.Append(ctx, repos, eventlog.AppendInput{
		EventType:  reviewFixBudgetHandoffEventType,
		ProjectID:  optionalBudgetString(projectID),
		LoopID:     optionalBudgetString(loopID),
		EntityType: optionalBudgetString("loop"),
		EntityID:   optionalBudgetString(loopID),
		ActorType:  optionalBudgetString("system"),
		ActorID:    optionalBudgetString("review-fix-budget"),
		Payload:    payload,
	})
}

// reviewFixBudgetHandoffResume returns concrete CLI commands including loop seq
// (fallback to id when seq is 0). HITL-on keeps Continue/Stop plus the same CLI.
func reviewFixBudgetHandoffResume(exhausted storage.LoopRecord, hitlEnabled bool) string {
	selector := strings.TrimSpace(exhausted.ID)
	if exhausted.Seq > 0 {
		selector = fmt.Sprintf("%d", exhausted.Seq)
	}
	cli := fmt.Sprintf("looper unpause %s / looper stop %s", selector, selector)
	if hitlEnabled {
		return ReviewFixBudgetAnswerContinue + " / " + ReviewFixBudgetAnswerStop + "; " + cli
	}
	return cli
}

// reviewFixBudgetHandoffHead prefers lastPublishedHeadSha (reviewer) then
// lastFixHeadSha (fixer) from exhausted-loop metadata.
func reviewFixBudgetHandoffHead(exhausted storage.LoopRecord) string {
	meta := parseMetadataObject(exhausted.MetadataJSON)
	if head, ok := stringFromAny(meta["lastPublishedHeadSha"]); ok && strings.TrimSpace(head) != "" {
		return strings.TrimSpace(head)
	}
	if head, ok := stringFromAny(meta["lastFixHeadSha"]); ok && strings.TrimSpace(head) != "" {
		return strings.TrimSpace(head)
	}
	return ""
}

func reviewFixBudgetHandoffMeters(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, input ParkReviewFixBudgetInput) (reviewerCount, reviewerCap, fixerCount, fixerCap int, exhaustedRoles []string) {
	reviewerCap = input.LiveCaps.ReviewerMaxPublishes
	fixerCap = input.LiveCaps.FixerMaxPushes
	if reviewerCap == 0 && strings.TrimSpace(exhausted.Type) == "reviewer" {
		reviewerCap = input.Cap
	}
	if fixerCap == 0 && strings.TrimSpace(exhausted.Type) == "fixer" {
		fixerCap = input.Cap
	}
	// Prefer live pair metadata over the single-role Count/Cap snapshot.
	members := []storage.LoopRecord{exhausted}
	if repos != nil && repos.Loops != nil {
		if all, err := repos.Loops.List(ctx); err == nil {
			members = reviewFixBudgetPairMembers(all, exhausted)
		}
	}
	for _, member := range members {
		switch strings.TrimSpace(member.Type) {
		case "reviewer":
			reviewerCount = ReviewerPublishCount(member.MetadataJSON)
		case "fixer":
			fixerCount = FixerPushCount(member.MetadataJSON)
		}
	}
	// Fall back to the triggering role's admission snapshot when pair read is empty.
	if strings.TrimSpace(exhausted.Type) == "reviewer" && reviewerCount == 0 && input.Count > 0 {
		reviewerCount = input.Count
	}
	if strings.TrimSpace(exhausted.Type) == "fixer" && fixerCount == 0 && input.Count > 0 {
		fixerCount = input.Count
	}
	if BudgetExhausted(reviewerCount, reviewerCap) {
		exhaustedRoles = append(exhaustedRoles, "reviewer")
	}
	if BudgetExhausted(fixerCount, fixerCap) {
		exhaustedRoles = append(exhaustedRoles, "fixer")
	}
	if len(exhaustedRoles) == 0 && strings.TrimSpace(input.Role) != "" {
		exhaustedRoles = []string{strings.TrimSpace(input.Role)}
	}
	return reviewerCount, reviewerCap, fixerCount, fixerCap, exhaustedRoles
}

func optionalBudgetString(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}

type ReviewFixBudgetAnswerResult struct {
	Applied bool
	Action  string
	Loop    storage.LoopRecord
}

// ApplyReviewFixBudgetAnswer handles Continue (reset at/over-cap meters, queue,
// release pair) or Stop (terminate both). Works for HITL asks and no-ask holds.
// Caps must be live config values used to decide which meters to refill.
// Returns Applied=false when the loop is not a review-fix budget hold.
func ApplyReviewFixBudgetAnswer(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, answer, nowISO string, caps ReviewFixBudgetLiveCaps) (ReviewFixBudgetAnswerResult, error) {
	if !IsReviewFixBudgetHold(loop) {
		return ReviewFixBudgetAnswerResult{}, nil
	}
	// Mid-run agent asks share awaiting_human but are not budget holds; the
	// IsReviewFixBudgetHold check above already requires a budget ask kind.
	if repos == nil || repos.Loops == nil {
		return ReviewFixBudgetAnswerResult{}, fmt.Errorf("review-fix budget answer requires loop storage")
	}
	if IsReviewFixBudgetStop(answer) {
		updated, err := terminateReviewFixPair(ctx, repos, loop, nowISO)
		if err != nil {
			return ReviewFixBudgetAnswerResult{}, err
		}
		return ReviewFixBudgetAnswerResult{Applied: true, Action: "stop", Loop: updated}, nil
	}
	if !IsReviewFixBudgetContinue(answer) {
		return ReviewFixBudgetAnswerResult{}, ErrReviewFixBudgetInvalidAnswer
	}
	updated, err := continueReviewFixBudget(ctx, repos, loop, nowISO, caps)
	if err != nil {
		return ReviewFixBudgetAnswerResult{}, err
	}
	return ReviewFixBudgetAnswerResult{Applied: true, Action: "continue", Loop: updated}, nil
}

func continueReviewFixBudget(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string, caps ReviewFixBudgetLiveCaps) (storage.LoopRecord, error) {
	// Keep holds until every required meter reset succeeds, then release the pair.
	// Fail-closed: never unpause a sibling until meters are reset and the
	// exhausted/answered side can complete its transition. Callers that have a
	// DB should wrap ApplyReviewFixBudgetAnswer in storage.WithTransaction so
	// release is atomic; without a TX we still release the answered side first
	// so a later sibling failure cannot leave a sibling queued while the
	// exhausted side remains held.
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return loop, err
	}
	members := reviewFixBudgetPairMembers(all, loop)

	for _, member := range members {
		fresh, getErr := repos.Loops.GetByID(ctx, member.ID)
		if getErr != nil {
			return loop, getErr
		}
		if fresh == nil {
			continue
		}
		if !IsReviewFixBudgetHold(*fresh) && fresh.ID != loop.ID {
			// Independently paused / failed siblings are not budget-held.
			if fresh.Status == "paused" && !IsSiblingReviewFixBudgetPause(fresh.MetadataJSON) && !IsReviewFixBudgetExhaustedPause(fresh.MetadataJSON) {
				continue
			}
			if fresh.Status == "failed" || fresh.Status == "interrupted" {
				continue
			}
		}
		encoded, resetErr := resetReviewFixBudgetMetersIfNeeded(fresh.MetadataJSON, fresh.Type, caps)
		if resetErr != nil {
			return loop, resetErr
		}
		if encoded == nil {
			continue
		}
		// Clear handoff marker on refill so a later park emits a fresh event.
		state := ReadReviewFixBudgetState(encoded)
		if state.HandoffEventAt != "" {
			state.HandoffEventAt = ""
			cleared, writeErr := WriteReviewFixBudgetState(encoded, state)
			if writeErr != nil {
				return loop, writeErr
			}
			encoded = &cleared
		}
		updated := *fresh
		updated.MetadataJSON = encoded
		updated.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return loop, err
		}
	}

	// Release answered/exhausted side first while siblings stay held.
	if err := releaseOneReviewFixBudgetHold(ctx, repos, loop.ID, nowISO); err != nil {
		return loop, err
	}
	for _, member := range members {
		if member.ID == loop.ID {
			continue
		}
		if err := releaseOneReviewFixBudgetHold(ctx, repos, member.ID, nowISO); err != nil {
			return loop, err
		}
	}
	fresh, err := repos.Loops.GetByID(ctx, loop.ID)
	if err != nil {
		return loop, err
	}
	if fresh == nil {
		return loop, fmt.Errorf("review-fix budget continue lost loop %s", loop.ID)
	}
	return *fresh, nil
}

func reviewFixBudgetPairMembers(all []storage.LoopRecord, loop storage.LoopRecord) []storage.LoopRecord {
	members := FindSiblingReviewFixLoops(all, loop)
	members = append(members, loop)
	return members
}

func resetReviewFixBudgetMetersIfNeeded(metadataJSON *string, loopType string, caps ReviewFixBudgetLiveCaps) (*string, error) {
	switch strings.TrimSpace(loopType) {
	case "reviewer":
		if !BudgetExhausted(ReviewerPublishCount(metadataJSON), caps.ReviewerMaxPublishes) {
			return nil, nil
		}
		encoded, err := ResetReviewerPublishCount(metadataJSON)
		if err != nil {
			return nil, err
		}
		// Clear exhausted markers on the meter owner when refilling.
		state := ReadReviewFixBudgetState(&encoded)
		state.ExhaustedBy = ""
		if state.PauseReason == ReviewFixBudgetTerminationReason {
			state.PauseReason = ""
		}
		encoded, err = WriteReviewFixBudgetState(&encoded, state)
		if err != nil {
			return nil, err
		}
		return &encoded, nil
	case "fixer":
		if !BudgetExhausted(FixerPushCount(metadataJSON), caps.FixerMaxPushes) {
			return nil, nil
		}
		encoded, err := ResetFixerPushCount(metadataJSON)
		if err != nil {
			return nil, err
		}
		return &encoded, nil
	default:
		return nil, nil
	}
}

func releaseOneReviewFixBudgetHold(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	fresh, err := repos.Loops.GetByID(ctx, loopID)
	if err != nil {
		return err
	}
	if fresh == nil {
		return nil
	}
	if !IsReviewFixBudgetHold(*fresh) {
		return nil
	}
	metadata := fresh.MetadataJSON
	if ask, ok := ReadHITLAsk(metadata); ok && IsReviewFixBudgetAsk(ask) {
		cleared, clearErr := ClearHITLAsk(metadata)
		if clearErr != nil {
			return clearErr
		}
		metadata = &cleared
	}
	state := ReadReviewFixBudgetState(metadata)
	state.SiblingOf = ""
	if state.PauseReason == ReviewFixBudgetPauseReason || state.PauseReason == ReviewFixBudgetTerminationReason {
		state.PauseReason = ""
	}
	state.ExhaustedBy = ""
	encoded, err := WriteReviewFixBudgetState(metadata, state)
	if err != nil {
		return err
	}
	meta := parseMetadataObject(&encoded)
	if reason, _ := stringFromAny(meta["pauseReason"]); reason == ReviewFixBudgetPauseReason || reason == ReviewFixBudgetTerminationReason {
		delete(meta, "pauseReason")
	}
	out, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	text := string(out)
	updated := *fresh
	updated.MetadataJSON = &text
	updated.Status = "queued"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	if err := requeueReviewFixBudgetLoop(ctx, repos, updated.ID, nowISO); err != nil {
		return err
	}
	if reviewFixBudgetReleaseHook != nil {
		if err := reviewFixBudgetReleaseHook(updated.ID); err != nil {
			return err
		}
	}
	return nil
}

func requeueReviewFixBudgetLoop(ctx context.Context, repos *storage.Repositories, loopID, nowISO string) error {
	if repos == nil || repos.Queue == nil || strings.TrimSpace(loopID) == "" {
		return nil
	}
	requeued, err := repos.Queue.RequeueLatestCancelledByLoop(ctx, loopID, nowISO)
	if err != nil {
		return err
	}
	if requeued > 0 {
		return nil
	}
	active, err := repos.Queue.FindActiveByLoopID(ctx, loopID)
	if err != nil || active != nil {
		return err
	}
	latest, err := repos.Queue.GetLatestByLoopID(ctx, loopID)
	if err != nil || latest == nil {
		return err
	}
	if latest.DedupeKey != "" {
		activeDedupe, dedupeErr := repos.Queue.FindActiveByDedupe(ctx, latest.DedupeKey)
		if dedupeErr != nil {
			return dedupeErr
		}
		if activeDedupe != nil {
			return nil
		}
	}
	replacement := *latest
	replacement.ID = eventlog.NewEventID("queue")
	replacement.Status = "queued"
	replacement.AvailableAt = nowISO
	replacement.Attempts = 0
	replacement.ClaimedBy = nil
	replacement.ClaimedAt = nil
	replacement.StartedAt = nil
	replacement.FinishedAt = nil
	replacement.LastError = nil
	replacement.LastErrorKind = nil
	replacement.CreatedAt = nowISO
	replacement.UpdatedAt = nowISO
	_, _, err = repos.Queue.UpsertActiveByDedupeOrGetExisting(ctx, replacement)
	return err
}

func terminateReviewFixPair(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	// Keep the answered/held loop until every same-lane sibling is terminated so
	// a later poll can retry the same Stop.
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return loop, err
	}
	for _, sibling := range FindSiblingReviewFixLoops(all, loop) {
		if _, err := terminateReviewFixLoop(ctx, repos, sibling, nowISO); err != nil {
			return loop, err
		}
	}
	return terminateReviewFixLoop(ctx, repos, loop, nowISO)
}

func terminateReviewFixLoop(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	switch loop.Status {
	case "terminated", "stopped":
		return loop, nil
	}
	meta := parseMetadataObject(loop.MetadataJSON)
	if loop.Type == "reviewer" {
		loopMeta, _ := meta[reviewerLoopMetadataKey].(map[string]any)
		if loopMeta == nil {
			loopMeta = map[string]any{}
		}
		loopMeta["status"] = "terminated"
		loopMeta["terminationReason"] = ReviewFixBudgetTerminationReason
		meta[reviewerLoopMetadataKey] = loopMeta
	}
	if reason, _ := stringFromAny(meta["pauseReason"]); reason == ReviewFixBudgetPauseReason || reason == ReviewFixBudgetTerminationReason {
		delete(meta, "pauseReason")
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		return loop, err
	}
	encodedText := string(encoded)
	cleared, err := ClearHITLAsk(&encodedText)
	if err != nil {
		return loop, err
	}
	state := ReadReviewFixBudgetState(&cleared)
	state.SiblingOf = ""
	state.ExhaustedBy = ""
	if state.PauseReason == ReviewFixBudgetPauseReason || state.PauseReason == ReviewFixBudgetTerminationReason {
		state.PauseReason = ""
	}
	cleared, err = WriteReviewFixBudgetState(&cleared, state)
	if err != nil {
		return loop, err
	}
	updated := loop
	updated.MetadataJSON = &cleared
	updated.Status = "terminated"
	updated.NextRunAt = nil
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return loop, err
	}
	if repos.Queue != nil {
		reason := ReviewFixBudgetTerminationReason
		if _, err := repos.Queue.CancelByLoop(ctx, updated.ID, nowISO, &reason); err != nil {
			return updated, err
		}
	}
	return updated, nil
}

func intFromMetadata(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func stringFromAny(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func derefLoopString(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func derefLoopInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
