package loops

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/powerformer/looper/internal/domain"
	"github.com/powerformer/looper/internal/eventlog"
	"github.com/powerformer/looper/internal/storage"
)

type Service struct {
	DB    *sql.DB
	Repos *storage.Repositories
	Now   func() time.Time
}

type CreateInput struct {
	ProjectID    string
	Type         domain.LoopType
	Target       domain.LoopTarget
	Status       domain.LoopStatus
	ConfigJSON   *string
	MetadataJSON *string
}

type TransitionInput struct {
	Status    domain.LoopStatus
	NextRunAt *time.Time
	LastRunAt *time.Time
}

type PauseResult struct {
	Loop                storage.LoopRecord
	CancelledQueueItems int64
}

func (s *Service) Create(ctx context.Context, input CreateInput) (storage.LoopRecord, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Loops == nil {
		return storage.LoopRecord{}, fmt.Errorf("loops service is not configured")
	}
	if err := domain.AssertLoopTypeMatchesTarget(input.Type, input.Target); err != nil {
		return storage.LoopRecord{}, err
	}

	now := s.currentTime()
	nowISO := eventlog.FormatJavaScriptISOString(now)

	record, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		project, err := repos.Projects.GetByID(ctx, input.ProjectID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if project == nil {
			return storage.LoopRecord{}, fmt.Errorf("project not found: %s", input.ProjectID)
		}

		existingLoops, err := repos.Loops.List(ctx)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		summaries := make([]domain.LoopSummary, 0, len(existingLoops))
		for _, loop := range existingLoops {
			summary, convErr := loopSummaryFromRecord(loop)
			if convErr != nil {
				return storage.LoopRecord{}, convErr
			}
			summaries = append(summaries, summary)
		}

		id := eventlog.NewEventID("loop")
		candidate := domain.LoopSummary{ID: id, ProjectID: input.ProjectID, Type: input.Type, Target: input.Target, Status: input.Status}
		if err := domain.AssertUniqueActiveLoop(summaries, candidate); err != nil {
			return storage.LoopRecord{}, err
		}

		seq, err := repos.Loops.AllocateSeq(ctx)
		if err != nil {
			return storage.LoopRecord{}, err
		}

		record := storage.LoopRecord{
			ID:           id,
			Seq:          seq,
			ProjectID:    input.ProjectID,
			Type:         string(input.Type),
			TargetType:   string(input.Target.TargetType),
			TargetID:     loopTargetID(input.Target),
			Repo:         repoFromTarget(input.Target),
			PRNumber:     prNumberFromTarget(input.Target),
			Status:       string(input.Status),
			ConfigJSON:   input.ConfigJSON,
			MetadataJSON: input.MetadataJSON,
			CreatedAt:    nowISO,
			UpdatedAt:    nowISO,
		}
		if input.Status == domain.LoopStatusRunning {
			record.NextRunAt = &nowISO
		}
		if err := repos.Loops.Upsert(ctx, record); err != nil {
			return storage.LoopRecord{}, err
		}

		return record, nil
	})
	if err != nil {
		return storage.LoopRecord{}, err
	}

	return record, nil
}

func (s *Service) Get(ctx context.Context, id string) (*storage.LoopRecord, error) {
	if s.Repos == nil || s.Repos.Loops == nil {
		return nil, fmt.Errorf("loops repository is not configured")
	}
	return s.Repos.Loops.GetByID(ctx, id)
}

func (s *Service) GetBySeq(ctx context.Context, seq int64) (*storage.LoopRecord, error) {
	if s.Repos == nil || s.Repos.Loops == nil {
		return nil, fmt.Errorf("loops repository is not configured")
	}
	return s.Repos.Loops.GetBySeq(ctx, seq)
}

func (s *Service) List(ctx context.Context) ([]storage.LoopRecord, error) {
	if s.Repos == nil || s.Repos.Loops == nil {
		return nil, fmt.Errorf("loops repository is not configured")
	}
	return s.Repos.Loops.List(ctx)
}

func (s *Service) TransitionStatus(ctx context.Context, loopID string, input TransitionInput) (storage.LoopRecord, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Loops == nil {
		return storage.LoopRecord{}, fmt.Errorf("loops service is not configured")
	}

	now := s.currentTime()
	record, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (storage.LoopRecord, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return storage.LoopRecord{}, err
		}
		if loop == nil {
			return storage.LoopRecord{}, fmt.Errorf("loop not found: %s", loopID)
		}
		if err := domain.AssertLoopStatusTransition(domain.LoopStatus(loop.Status), input.Status); err != nil {
			return storage.LoopRecord{}, err
		}

		updated := *loop
		updated.Status = string(input.Status)
		updated.UpdatedAt = eventlog.FormatJavaScriptISOString(now)
		if input.NextRunAt != nil {
			nextRunAt := eventlog.FormatJavaScriptISOString(*input.NextRunAt)
			updated.NextRunAt = &nextRunAt
		} else if input.Status == domain.LoopStatusQueued {
			nextRunAt := updated.UpdatedAt
			updated.NextRunAt = &nextRunAt
		} else if input.Status != domain.LoopStatusRunning {
			updated.NextRunAt = nil
		}
		if input.LastRunAt != nil {
			lastRunAt := eventlog.FormatJavaScriptISOString(*input.LastRunAt)
			updated.LastRunAt = &lastRunAt
		}
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return storage.LoopRecord{}, err
		}
		return updated, nil
	})
	if err != nil {
		return storage.LoopRecord{}, err
	}
	return record, nil
}

func (s *Service) Pause(ctx context.Context, loopID string, reason *string) (PauseResult, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Queue == nil {
		return PauseResult{}, fmt.Errorf("loops service is not configured")
	}
	now := s.currentTime()
	result, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (PauseResult, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, loopID)
		if err != nil {
			return PauseResult{}, err
		}
		if loop == nil {
			return PauseResult{}, fmt.Errorf("loop not found: %s", loopID)
		}
		currentStatus := domain.LoopStatus(loop.Status)
		if currentStatus != domain.LoopStatusPaused {
			if err := domain.AssertLoopStatusTransition(currentStatus, domain.LoopStatusPaused); err != nil {
				return PauseResult{}, err
			}
		}
		updated := *loop
		updated.Status = string(domain.LoopStatusPaused)
		updated.NextRunAt = nil
		updated.UpdatedAt = eventlog.FormatJavaScriptISOString(now)
		if err := repos.Loops.Upsert(ctx, updated); err != nil {
			return PauseResult{}, err
		}
		cancelled, err := repos.Queue.CancelByLoop(ctx, loopID, updated.UpdatedAt, reason)
		if err != nil {
			return PauseResult{}, err
		}
		return PauseResult{Loop: updated, CancelledQueueItems: cancelled}, nil
	})
	if err != nil {
		return PauseResult{}, err
	}
	return result, nil
}

func (s *Service) Resume(ctx context.Context, loopID string) (storage.LoopRecord, error) {
	now := s.currentTime()
	return s.TransitionStatus(ctx, loopID, TransitionInput{Status: domain.LoopStatusQueued, NextRunAt: &now})
}

func (s *Service) currentTime() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func loopSummaryFromRecord(record storage.LoopRecord) (domain.LoopSummary, error) {
	target, err := targetFromRecord(record)
	if err != nil {
		return domain.LoopSummary{}, err
	}
	return domain.LoopSummary{ID: record.ID, ProjectID: record.ProjectID, Type: domain.LoopType(record.Type), Target: target, Status: domain.LoopStatus(record.Status)}, nil
}

func targetFromRecord(record storage.LoopRecord) (domain.LoopTarget, error) {
	target := domain.LoopTarget{TargetType: domain.LoopTargetType(record.TargetType)}
	switch target.TargetType {
	case domain.LoopTargetTypeProject:
		if record.TargetID == nil {
			return domain.LoopTarget{}, fmt.Errorf("project loop %s has no target id", record.ID)
		}
		target.ProjectID = trimTargetPrefix(*record.TargetID)
	case domain.LoopTargetTypeIssue:
		if record.Repo == nil || record.TargetID == nil {
			return domain.LoopTarget{}, fmt.Errorf("issue loop %s is missing target data", record.ID)
		}
		target.Repo = *record.Repo
		index := strings.LastIndex(*record.TargetID, ":")
		if index < 0 || index+1 >= len(*record.TargetID) {
			return domain.LoopTarget{}, fmt.Errorf("issue loop %s has invalid target id %q", record.ID, *record.TargetID)
		}
		issueNumber := (*record.TargetID)[index+1:]
		_, scanErr := fmt.Sscanf(issueNumber, "%d", &target.IssueNumber)
		if scanErr != nil {
			return domain.LoopTarget{}, fmt.Errorf("issue loop %s has invalid issue number: %w", record.ID, scanErr)
		}
	default:
		if record.Repo == nil || record.PRNumber == nil {
			return domain.LoopTarget{}, fmt.Errorf("pull request loop %s is missing target data", record.ID)
		}
		target.Repo = *record.Repo
		target.PRNumber = *record.PRNumber
	}
	return target, nil
}

func loopTargetID(target domain.LoopTarget) *string {
	value := domain.LoopTargetKey(target)
	return &value
}

func repoFromTarget(target domain.LoopTarget) *string {
	if target.TargetType == domain.LoopTargetTypeProject {
		return nil
	}
	return &target.Repo
}

func prNumberFromTarget(target domain.LoopTarget) *int64 {
	if target.TargetType != domain.LoopTargetTypePullRequest {
		return nil
	}
	return &target.PRNumber
}

func trimTargetPrefix(targetID string) string {
	normalized := strings.TrimSpace(targetID)
	for strings.HasPrefix(normalized, "project:") {
		normalized = strings.TrimPrefix(normalized, "project:")
	}
	return normalized
}

func stringPointer(value string) *string {
	return &value
}
