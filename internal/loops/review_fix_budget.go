package loops

import (
	"context"
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
	fixerPushCountKey          = "pushCount"
)

// ErrReviewFixBudgetInvalidAnswer is the only ApplyReviewFixBudgetAnswer
// failure that API clients should treat as a 400 validation error.
var ErrReviewFixBudgetInvalidAnswer = fmt.Errorf("review-fix budget answer must be %q or %q", ReviewFixBudgetAnswerContinue, ReviewFixBudgetAnswerStop)

// ReviewFixBudgetState is the durable PR-scoped ledger for the new caps.
// Authority for the cap is live config; this record only stores counts and
// which role exhausted.
type ReviewFixBudgetState struct {
	PushCount   int    `json:"pushCount,omitempty"`
	ExhaustedBy string `json:"exhaustedBy,omitempty"`
	SiblingOf   string `json:"siblingOf,omitempty"`
	PauseReason string `json:"pauseReason,omitempty"`
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

func IsSiblingReviewFixBudgetPause(metadataJSON *string) bool {
	if reason, _ := stringFromAny(parseMetadataObject(metadataJSON)["pauseReason"]); reason == ReviewFixBudgetPauseReason {
		return true
	}
	return ReadReviewFixBudgetState(metadataJSON).PauseReason == ReviewFixBudgetPauseReason
}

func isManualReviewFixLoop(loop storage.LoopRecord) bool {
	manual, _ := parseMetadataObject(loop.MetadataJSON)["manual"].(bool)
	return manual
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

// FindSiblingReviewFixLoops returns automatic opposite-role loops on the same
// project/repo/PR. Manual loops are not part of the review-fix budget pair.
func FindSiblingReviewFixLoops(all []storage.LoopRecord, loop storage.LoopRecord) []storage.LoopRecord {
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
		if isManualReviewFixLoop(candidate) {
			continue
		}
		siblings = append(siblings, candidate)
	}
	return siblings
}

// FindSiblingReviewFixLoop returns the preferred automatic sibling: a
// non-terminal match when one exists, otherwise the first automatic match.
func FindSiblingReviewFixLoop(all []storage.LoopRecord, loop storage.LoopRecord) *storage.LoopRecord {
	siblings := FindSiblingReviewFixLoops(all, loop)
	for i := range siblings {
		if !isTerminalReviewFixSiblingStatus(siblings[i].Status) {
			return &siblings[i]
		}
	}
	if len(siblings) > 0 {
		return &siblings[0]
	}
	return nil
}

type ParkReviewFixBudgetInput struct {
	Exhausted storage.LoopRecord
	Role      string
	Repo      string
	PRNumber  int64
	Count     int
	Cap       int
	NowISO    string
}

// ParkReviewFixBudget parks the exhausted loop as awaiting_human with a budget
// HITL ask and pauses the sibling loop on the same PR. Queue items on both
// sides are cancelled so discovery cannot immediately restart the ping-pong.
// Re-entry on an already-awaiting budget loop finishes cancel/sibling park
// without rewriting a delivered ask.
func ParkReviewFixBudget(ctx context.Context, repos *storage.Repositories, input ParkReviewFixBudgetInput) (storage.LoopRecord, error) {
	if repos == nil || repos.Loops == nil {
		return input.Exhausted, fmt.Errorf("review-fix budget park requires loop storage")
	}
	exhausted := input.Exhausted
	if ask, ok := ReadHITLAsk(exhausted.MetadataJSON); ok && IsReviewFixBudgetAsk(ask) && exhausted.Status == "awaiting_human" {
		if err := finishReviewFixBudgetPark(ctx, repos, exhausted, input.Role, input.NowISO); err != nil {
			return exhausted, err
		}
		return exhausted, nil
	}
	ask := NewReviewFixBudgetAsk(input.Role, input.Repo, input.PRNumber, input.Count, input.Cap, input.NowISO)
	metadata, err := WriteHITLAsk(exhausted.MetadataJSON, ask)
	if err != nil {
		return input.Exhausted, err
	}
	state := ReadReviewFixBudgetState(&metadata)
	state.ExhaustedBy = strings.TrimSpace(input.Role)
	metadata, err = WriteReviewFixBudgetState(&metadata, state)
	if err != nil {
		return input.Exhausted, err
	}
	exhausted.MetadataJSON = &metadata
	exhausted.Status = "awaiting_human"
	exhausted.NextRunAt = nil
	exhausted.UpdatedAt = input.NowISO
	if err := repos.Loops.Upsert(ctx, exhausted); err != nil {
		return input.Exhausted, err
	}
	if err := finishReviewFixBudgetPark(ctx, repos, exhausted, input.Role, input.NowISO); err != nil {
		return exhausted, err
	}
	return exhausted, nil
}

func finishReviewFixBudgetPark(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, exhaustedBy, nowISO string) error {
	if repos.Queue != nil {
		reason := ReviewFixBudgetTerminationReason
		if _, err := repos.Queue.CancelByLoop(ctx, exhausted.ID, nowISO, &reason); err != nil {
			return err
		}
	}
	return parkSiblingReviewFixLoop(ctx, repos, exhausted, exhaustedBy, nowISO)
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

func parkOneSiblingReviewFixLoop(ctx context.Context, repos *storage.Repositories, sibling storage.LoopRecord, exhaustedBy, nowISO string) error {
	if !isReviewFixBudgetPauseApplicable(sibling.Status) {
		return nil
	}
	if sibling.Status == "paused" && !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
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

type ReviewFixBudgetAnswerResult struct {
	Applied bool
	Action  string
	Loop    storage.LoopRecord
}

// ApplyReviewFixBudgetAnswer handles Continue (reset counter, queue, unpause
// sibling) or Stop (terminate both). Returns Applied=false for mid-run HITL asks.
func ApplyReviewFixBudgetAnswer(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, answer, nowISO string) (ReviewFixBudgetAnswerResult, error) {
	ask, ok := ReadHITLAsk(loop.MetadataJSON)
	if !ok || !IsReviewFixBudgetAsk(ask) {
		return ReviewFixBudgetAnswerResult{}, nil
	}
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
	updated, err := continueReviewFixBudget(ctx, repos, loop, nowISO)
	if err != nil {
		return ReviewFixBudgetAnswerResult{}, err
	}
	return ReviewFixBudgetAnswerResult{Applied: true, Action: "continue", Loop: updated}, nil
}

func continueReviewFixBudget(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	// Keep awaiting_human + the budget ask until sibling unpause and requeue
	// succeed so a later GitHub/Feishu poll can retry the same answer.
	if err := unpauseSiblingReviewFixLoop(ctx, repos, loop, nowISO); err != nil {
		return loop, err
	}
	if err := requeueReviewFixBudgetLoop(ctx, repos, loop.ID, nowISO); err != nil {
		return loop, err
	}
	metadata := loop.MetadataJSON
	switch loop.Type {
	case "reviewer":
		encoded, resetErr := ResetReviewerPublishCount(metadata)
		if resetErr != nil {
			return loop, resetErr
		}
		metadata = &encoded
	case "fixer":
		encoded, resetErr := ResetFixerPushCount(metadata)
		if resetErr != nil {
			return loop, resetErr
		}
		metadata = &encoded
	}
	cleared, err := ClearHITLAsk(metadata)
	if err != nil {
		return loop, err
	}
	state := ReadReviewFixBudgetState(&cleared)
	state.ExhaustedBy = ""
	cleared, err = WriteReviewFixBudgetState(&cleared, state)
	if err != nil {
		return loop, err
	}
	updated := loop
	updated.MetadataJSON = &cleared
	updated.Status = "queued"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return loop, err
	}
	return updated, nil
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

func unpauseSiblingReviewFixLoop(ctx context.Context, repos *storage.Repositories, exhausted storage.LoopRecord, nowISO string) error {
	all, err := repos.Loops.List(ctx)
	if err != nil {
		return err
	}
	for _, sibling := range FindSiblingReviewFixLoops(all, exhausted) {
		if sibling.Status != "paused" || !IsSiblingReviewFixBudgetPause(sibling.MetadataJSON) {
			continue
		}
		if err := unpauseOneSiblingReviewFixLoop(ctx, repos, sibling, nowISO); err != nil {
			return err
		}
	}
	return nil
}

func unpauseOneSiblingReviewFixLoop(ctx context.Context, repos *storage.Repositories, sibling storage.LoopRecord, nowISO string) error {
	state := ReadReviewFixBudgetState(sibling.MetadataJSON)
	state.SiblingOf = ""
	state.PauseReason = ""
	metadata, err := WriteReviewFixBudgetState(sibling.MetadataJSON, state)
	if err != nil {
		return err
	}
	meta := parseMetadataObject(&metadata)
	delete(meta, "pauseReason")
	encoded, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	text := string(encoded)
	updated := sibling
	updated.MetadataJSON = &text
	updated.Status = "queued"
	updated.NextRunAt = &nowISO
	updated.UpdatedAt = nowISO
	if err := repos.Loops.Upsert(ctx, updated); err != nil {
		return err
	}
	return requeueReviewFixBudgetLoop(ctx, repos, updated.ID, nowISO)
}

func terminateReviewFixPair(ctx context.Context, repos *storage.Repositories, loop storage.LoopRecord, nowISO string) (storage.LoopRecord, error) {
	// Keep awaiting_human + the budget ask until every automatic sibling is
	// terminated so a later GitHub/Feishu poll can retry the same Stop.
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
	encoded, err := json.Marshal(meta)
	if err != nil {
		return loop, err
	}
	encodedText := string(encoded)
	cleared, err := ClearHITLAsk(&encodedText)
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
