package worktreecleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

type Service struct {
	Repos  *storage.Repositories
	Config config.WorktreeCleanupConfig
	Now    func() time.Time
}

type PlanResult struct {
	Summary   PlanSummary
	Decisions []Decision
}

type PlanSummary struct {
	Scanned    int
	Candidates int
	WouldClean int
	Skipped    int
	Failed     int
	Orphans    int
}

type Decision struct {
	Worktree   storage.WorktreeRecord
	Action     string
	Reason     string
	LastUsedAt *time.Time
	Orphan     bool
	References []Reference
}

type Reference struct {
	Kind   string
	ID     string
	Status string
}

type worktreeRef struct {
	ProjectID string
	ID        string
	Path      string
	Branch    string
}

type candidateState struct {
	worktree      storage.WorktreeRecord
	references    []Reference
	blocked       bool
	blockReason   string
	parseFailed   bool
	lastUsedAt    time.Time
	hasLastUsedAt bool
}

const (
	ActionWouldClean = "would_clean"
	ActionSkipped    = "skipped"
)

func (s *Service) Plan(ctx context.Context) (PlanResult, error) {
	if s == nil || s.Repos == nil || s.Repos.Worktrees == nil || s.Repos.Loops == nil || s.Repos.Runs == nil || s.Repos.Queue == nil {
		return PlanResult{}, fmt.Errorf("worktree cleanup service is not configured")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	retentionCutoff := now().UTC().Add(-time.Duration(s.Config.RetentionDays) * 24 * time.Hour)

	worktrees, err := s.Repos.Worktrees.ListActive(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	loops, err := s.Repos.Loops.List(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	runs, err := s.Repos.Runs.List(ctx)
	if err != nil {
		return PlanResult{}, err
	}
	queueItems, err := s.Repos.Queue.List(ctx)
	if err != nil {
		return PlanResult{}, err
	}

	states := make([]candidateState, 0, len(worktrees))
	for _, worktree := range worktrees {
		state := candidateState{worktree: worktree}
		state.noteTime(worktree.UpdatedAt)
		state.noteTime(worktree.CreatedAt)
		states = append(states, state)
	}

	loopsByID := make(map[string]storage.LoopRecord, len(loops))
	for _, loop := range loops {
		loopsByID[loop.ID] = loop
		ref, parseFailed := refFromJSON(loop.MetadataJSON)
		if parseFailed {
			markProjectParseFailure(states, loop.ProjectID, Reference{Kind: "loop", ID: loop.ID, Status: loop.Status})
			continue
		}
		ref = fillRefFromLoop(ref, loop)
		for index := range states {
			if !matchesRef(states[index].worktree, ref) {
				continue
			}
			states[index].references = append(states[index].references, Reference{Kind: "loop", ID: loop.ID, Status: loop.Status})
			states[index].noteTime(loop.UpdatedAt)
			if loop.LastRunAt != nil {
				states[index].noteTime(*loop.LastRunAt)
			}
			if protectsLoopStatus(loop.Status) {
				states[index].block("referenced by protected loop status " + loop.Status)
			}
		}
	}

	for _, run := range runs {
		loop, ok := loopsByID[run.LoopID]
		ref := worktreeRef{}
		parseFailed := false
		if run.CheckpointJSON != nil {
			ref, parseFailed = refFromJSON(run.CheckpointJSON)
		}
		if parseFailed {
			projectID := ""
			ref := worktreeRef{}
			if ok {
				projectID = loop.ProjectID
				ref = fillRefFromLoop(ref, loop)
			}
			markParseFailure(states, projectID, ref, Reference{Kind: "run", ID: run.ID, Status: run.Status})
			continue
		}
		if ok {
			ref = fillRefFromLoop(ref, loop)
		}
		for index := range states {
			if !matchesRef(states[index].worktree, ref) {
				continue
			}
			states[index].references = append(states[index].references, Reference{Kind: "run", ID: run.ID, Status: run.Status})
			states[index].noteTime(run.UpdatedAt)
			states[index].noteTime(run.StartedAt)
			if run.EndedAt != nil {
				states[index].noteTime(*run.EndedAt)
			}
			if run.Status == "running" {
				states[index].block("referenced by running run")
			}
		}
	}

	for _, item := range queueItems {
		if item.Status != "queued" && item.Status != "running" {
			continue
		}
		ref, parseFailed := refFromJSON(item.PayloadJSON)
		if parseFailed {
			continue
		}
		if item.LoopID != nil {
			if loop, ok := loopsByID[*item.LoopID]; ok {
				ref = fillRefFromLoop(ref, loop)
			}
		} else if item.ProjectID != nil && ref.ProjectID == "" {
			ref.ProjectID = *item.ProjectID
		}
		for index := range states {
			if !matchesRef(states[index].worktree, ref) {
				continue
			}
			states[index].references = append(states[index].references, Reference{Kind: "queue", ID: item.ID, Status: item.Status})
			states[index].noteTime(item.UpdatedAt)
			states[index].block("referenced by active queue item")
		}
	}

	result := PlanResult{Summary: PlanSummary{Scanned: len(states)}}
	wouldClean := 0
	for _, state := range states {
		decision := Decision{Worktree: state.worktree, References: state.references}
		if state.hasLastUsedAt {
			lastUsedAt := state.lastUsedAt
			decision.LastUsedAt = &lastUsedAt
		}
		if len(state.references) == 0 {
			decision.Orphan = true
			result.Summary.Orphans++
			if !s.Config.IncludeOrphans {
				decision.Action = ActionSkipped
				decision.Reason = "orphan worktree and includeOrphans=false"
				result.Summary.Skipped++
				result.Decisions = append(result.Decisions, decision)
				continue
			}
		}
		if state.parseFailed {
			decision.Action = ActionSkipped
			decision.Reason = state.blockReason
			result.Summary.Skipped++
			result.Summary.Failed++
			result.Decisions = append(result.Decisions, decision)
			continue
		}
		if state.blocked {
			decision.Action = ActionSkipped
			decision.Reason = state.blockReason
			result.Summary.Skipped++
			result.Decisions = append(result.Decisions, decision)
			continue
		}
		if state.hasLastUsedAt && state.lastUsedAt.After(retentionCutoff) {
			decision.Action = ActionSkipped
			decision.Reason = "within retention window"
			result.Summary.Skipped++
			result.Decisions = append(result.Decisions, decision)
			continue
		}
		result.Summary.Candidates++
		if wouldClean >= s.Config.MaxPerTick {
			decision.Action = ActionSkipped
			decision.Reason = "maxPerTick limit reached"
			result.Summary.Skipped++
			result.Decisions = append(result.Decisions, decision)
			continue
		}
		decision.Action = ActionWouldClean
		decision.Reason = "eligible in dry-run plan"
		result.Summary.WouldClean++
		wouldClean++
		result.Decisions = append(result.Decisions, decision)
	}

	return result, nil
}

func (s *candidateState) noteTime(value string) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return
	}
	if !s.hasLastUsedAt || parsed.After(s.lastUsedAt) {
		s.lastUsedAt = parsed
		s.hasLastUsedAt = true
	}
}

func (s *candidateState) block(reason string) {
	if s.blocked {
		return
	}
	s.blocked = true
	s.blockReason = reason
}

func protectsLoopStatus(status string) bool {
	switch status {
	case "idle", "queued", "running", "waiting", "paused", "failed", "interrupted":
		return true
	default:
		return false
	}
}

func fillRefFromLoop(ref worktreeRef, loop storage.LoopRecord) worktreeRef {
	if ref.ProjectID == "" {
		ref.ProjectID = loop.ProjectID
	}
	if loop.MetadataJSON != nil {
		metadataRef, _ := refFromJSON(loop.MetadataJSON)
		if ref.ProjectID == "" {
			ref.ProjectID = metadataRef.ProjectID
		}
		if ref.ID == "" {
			ref.ID = metadataRef.ID
		}
		if ref.Path == "" {
			ref.Path = metadataRef.Path
		}
		if ref.Branch == "" {
			ref.Branch = metadataRef.Branch
		}
	}
	return ref
}

func refFromJSON(raw *string) (worktreeRef, bool) {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return worktreeRef{}, false
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(*raw), &value); err != nil {
		return worktreeRef{}, true
	}
	ref := refFromMap(value)
	if nested, ok := value["worktree"].(map[string]any); ok {
		nestedRef := refFromMap(nested)
		if ref.ProjectID == "" {
			ref.ProjectID = nestedRef.ProjectID
		}
		if ref.ID == "" {
			ref.ID = nestedRef.ID
		}
		if ref.Path == "" {
			ref.Path = nestedRef.Path
		}
		if ref.Branch == "" {
			ref.Branch = nestedRef.Branch
		}
	}
	return ref, false
}

func refFromMap(value map[string]any) worktreeRef {
	return worktreeRef{
		ProjectID: firstString(value["projectId"], value["projectID"], value["project_id"]),
		ID:        firstString(value["worktreeId"], value["id"]),
		Path:      firstString(value["worktreePath"], value["path"]),
		Branch:    firstString(value["branch"]),
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}

func matchesRef(worktree storage.WorktreeRecord, ref worktreeRef) bool {
	if ref.ID != "" {
		return ref.ID == worktree.ID
	}
	if ref.Path != "" {
		return ref.Path == worktree.WorktreePath
	}
	if ref.Branch == "" {
		return false
	}
	if ref.ProjectID != "" && ref.ProjectID != worktree.ProjectID {
		return false
	}
	return ref.Branch == worktree.Branch
}

func markProjectParseFailure(states []candidateState, projectID string, reference Reference) {
	markParseFailure(states, projectID, worktreeRef{}, reference)
}

func markParseFailure(states []candidateState, projectID string, ref worktreeRef, reference Reference) {
	for index := range states {
		if projectID != "" && states[index].worktree.ProjectID != projectID {
			continue
		}
		if (ref != worktreeRef{}) && !matchesRef(states[index].worktree, ref) {
			continue
		}
		states[index].parseFailed = true
		states[index].references = append(states[index].references, reference)
		states[index].block("checkpoint parse failure")
	}
}
