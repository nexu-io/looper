package runs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/nexu-io/looper/internal/domain"
	"github.com/nexu-io/looper/internal/eventlog"
	"github.com/nexu-io/looper/internal/loops"
	"github.com/nexu-io/looper/internal/storage"
)

type Service struct {
	DB    *sql.DB
	Repos *storage.Repositories
	Loops *loops.Service
	Now   func() time.Time
}

type StartInput struct {
	LoopID            string
	CurrentStep       *string
	LastCompletedStep *string
	CheckpointJSON    *string
}

type RecordStepInput struct {
	RunID             string
	LoopType          domain.LoopType
	CurrentStep       *string
	LastCompletedStep *string
	CheckpointJSON    *string
	LastHeartbeatAt   *time.Time
	EventType         string
	EventPayload      any
}

type CompleteInput struct {
	Status         domain.RunStatus
	Summary        *string
	ErrorMessage   *string
	CheckpointJSON *string
}

func (s *Service) StartRun(ctx context.Context, input StartInput) (storage.RunRecord, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Runs == nil || s.Loops == nil {
		return storage.RunRecord{}, fmt.Errorf("runs service is not configured")
	}
	now := s.currentTime()
	nowISO := eventlog.FormatJavaScriptISOString(now)

	run, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (storage.RunRecord, error) {
		repos := storage.NewRepositories(tx)
		loop, err := repos.Loops.GetByID(ctx, input.LoopID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		if loop == nil {
			return storage.RunRecord{}, fmt.Errorf("loop not found: %s", input.LoopID)
		}
		hasRunningRun, err := repos.Runs.HasRunningByLoopID(ctx, input.LoopID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		if hasRunningRun {
			return storage.RunRecord{}, fmt.Errorf("loop %s already has a running run", input.LoopID)
		}
		if loop.Status != string(domain.LoopStatusRunning) {
			if err := domain.AssertLoopStatusTransition(domain.LoopStatus(loop.Status), domain.LoopStatusRunning); err != nil {
				return storage.RunRecord{}, err
			}
		}

		if input.CurrentStep != nil {
			if err := domain.AssertStepBelongsToLoopType(domain.LoopType(loop.Type), *input.CurrentStep); err != nil {
				return storage.RunRecord{}, err
			}
		}
		if input.LastCompletedStep != nil {
			if err := domain.AssertStepBelongsToLoopType(domain.LoopType(loop.Type), *input.LastCompletedStep); err != nil {
				return storage.RunRecord{}, err
			}
		}

		run := storage.RunRecord{
			ID:                eventlog.NewEventID("run"),
			LoopID:            input.LoopID,
			Status:            string(domain.RunStatusRunning),
			CurrentStep:       input.CurrentStep,
			LastCompletedStep: input.LastCompletedStep,
			CheckpointJSON:    input.CheckpointJSON,
			StartedAt:         nowISO,
			LastHeartbeatAt:   &nowISO,
			CreatedAt:         nowISO,
			UpdatedAt:         nowISO,
		}
		if err := repos.Runs.Upsert(ctx, run); err != nil {
			return storage.RunRecord{}, err
		}

		updatedLoop := *loop
		updatedLoop.Status = string(domain.LoopStatusRunning)
		updatedLoop.LastRunAt = &nowISO
		updatedLoop.NextRunAt = nil
		updatedLoop.UpdatedAt = nowISO
		if err := repos.Loops.Upsert(ctx, updatedLoop); err != nil {
			return storage.RunRecord{}, err
		}

		if err := eventlog.Append(ctx, repos, eventlog.AppendInput{
			EventType:  "loop.started",
			ProjectID:  &updatedLoop.ProjectID,
			LoopID:     &updatedLoop.ID,
			EntityType: stringPointer("loop"),
			EntityID:   &updatedLoop.ID,
			Payload:    map[string]any{"status": updatedLoop.Status},
			CreatedAt:  now,
		}); err != nil {
			return storage.RunRecord{}, err
		}
		if err := eventlog.Append(ctx, repos, eventlog.AppendInput{
			EventType:  "run.started",
			ProjectID:  &updatedLoop.ProjectID,
			LoopID:     &updatedLoop.ID,
			RunID:      &run.ID,
			EntityType: stringPointer("run"),
			EntityID:   &run.ID,
			Payload: map[string]any{
				"currentStep":       input.CurrentStep,
				"lastCompletedStep": input.LastCompletedStep,
			},
			CreatedAt: now,
		}); err != nil {
			return storage.RunRecord{}, err
		}

		return run, nil
	})
	if err != nil {
		return storage.RunRecord{}, err
	}
	return run, nil
}

func (s *Service) RecordStep(ctx context.Context, input RecordStepInput) (storage.RunRecord, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Runs == nil {
		return storage.RunRecord{}, fmt.Errorf("runs service is not configured")
	}
	now := s.currentTime()
	run, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (storage.RunRecord, error) {
		repos := storage.NewRepositories(tx)
		record, err := repos.Runs.GetByID(ctx, input.RunID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		if record == nil {
			return storage.RunRecord{}, fmt.Errorf("run not found: %s", input.RunID)
		}
		if input.CurrentStep != nil {
			if err := domain.AssertStepBelongsToLoopType(input.LoopType, *input.CurrentStep); err != nil {
				return storage.RunRecord{}, err
			}
		}
		if input.LastCompletedStep != nil {
			if err := domain.AssertStepBelongsToLoopType(input.LoopType, *input.LastCompletedStep); err != nil {
				return storage.RunRecord{}, err
			}
		}
		updated := *record
		updated.CurrentStep = input.CurrentStep
		updated.LastCompletedStep = input.LastCompletedStep
		updated.CheckpointJSON = input.CheckpointJSON
		heartbeatAt := now
		if input.LastHeartbeatAt != nil {
			heartbeatAt = input.LastHeartbeatAt.UTC()
		}
		heartbeatISO := eventlog.FormatJavaScriptISOString(heartbeatAt)
		updated.LastHeartbeatAt = &heartbeatISO
		updated.UpdatedAt = heartbeatISO
		if err := repos.Runs.Upsert(ctx, updated); err != nil {
			return storage.RunRecord{}, err
		}
		if input.EventType != "" {
			loop, loopErr := repos.Loops.GetByID(ctx, updated.LoopID)
			if loopErr != nil {
				return storage.RunRecord{}, loopErr
			}
			if loop != nil {
				if err := eventlog.Append(ctx, repos, eventlog.AppendInput{
					EventType:  input.EventType,
					ProjectID:  &loop.ProjectID,
					LoopID:     &loop.ID,
					RunID:      &updated.ID,
					EntityType: stringPointer("run"),
					EntityID:   &updated.ID,
					Payload:    input.EventPayload,
					CreatedAt:  heartbeatAt,
				}); err != nil {
					return storage.RunRecord{}, err
				}
			}
		}
		return updated, nil
	})
	if err != nil {
		return storage.RunRecord{}, err
	}
	return run, nil
}

func (s *Service) Complete(ctx context.Context, runID string, input CompleteInput) (storage.RunRecord, error) {
	if s.DB == nil || s.Repos == nil || s.Repos.Runs == nil {
		return storage.RunRecord{}, fmt.Errorf("runs service is not configured")
	}
	now := s.currentTime()
	run, err := storage.WithTransactionValue(ctx, s.DB, nil, func(tx *sql.Tx) (storage.RunRecord, error) {
		repos := storage.NewRepositories(tx)
		record, err := repos.Runs.GetByID(ctx, runID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		if record == nil {
			return storage.RunRecord{}, fmt.Errorf("run not found: %s", runID)
		}
		if err := domain.AssertRunStatusTransition(domain.RunStatus(record.Status), input.Status); err != nil {
			return storage.RunRecord{}, err
		}
		endedAt := eventlog.FormatJavaScriptISOString(now)
		updated := *record
		updated.Status = string(input.Status)
		updated.Summary = input.Summary
		updated.ErrorMessage = input.ErrorMessage
		updated.CheckpointJSON = input.CheckpointJSON
		updated.EndedAt = &endedAt
		updated.LastHeartbeatAt = &endedAt
		updated.UpdatedAt = endedAt
		if err := repos.Runs.Upsert(ctx, updated); err != nil {
			return storage.RunRecord{}, err
		}
		loop, err := repos.Loops.GetByID(ctx, updated.LoopID)
		if err != nil {
			return storage.RunRecord{}, err
		}
		if loop != nil {
			eventType := "run.completed"
			if input.Status != domain.RunStatusSuccess {
				eventType = "run.failed"
			}
			payload := map[string]any{}
			if input.Summary != nil {
				payload["summary"] = *input.Summary
			}
			if input.ErrorMessage != nil {
				payload["errorMessage"] = *input.ErrorMessage
			}
			if err := eventlog.Append(ctx, repos, eventlog.AppendInput{
				EventType:  eventType,
				ProjectID:  &loop.ProjectID,
				LoopID:     &loop.ID,
				RunID:      &updated.ID,
				EntityType: stringPointer("run"),
				EntityID:   &updated.ID,
				Payload:    payload,
				CreatedAt:  now,
			}); err != nil {
				return storage.RunRecord{}, err
			}
		}
		return updated, nil
	})
	if err != nil {
		return storage.RunRecord{}, err
	}
	return run, nil
}

func (s *Service) Get(ctx context.Context, id string) (*storage.RunRecord, error) {
	if s.Repos == nil || s.Repos.Runs == nil {
		return nil, fmt.Errorf("runs repository is not configured")
	}
	return s.Repos.Runs.GetByID(ctx, id)
}

func (s *Service) List(ctx context.Context) ([]storage.RunRecord, error) {
	if s.Repos == nil || s.Repos.Runs == nil {
		return nil, fmt.Errorf("runs repository is not configured")
	}
	return s.Repos.Runs.List(ctx)
}

func (s *Service) ListByLoop(ctx context.Context, loopID string) ([]storage.RunRecord, error) {
	if s.Repos == nil || s.Repos.Runs == nil {
		return nil, fmt.Errorf("runs repository is not configured")
	}
	return s.Repos.Runs.ListByLoop(ctx, loopID)
}

func (s *Service) LatestForLoop(ctx context.Context, loopID string) (*storage.RunRecord, error) {
	if s.Repos == nil || s.Repos.Runs == nil {
		return nil, fmt.Errorf("runs repository is not configured")
	}
	return s.Repos.Runs.GetLatestByLoopID(ctx, loopID)
}

func (s *Service) currentTime() time.Time {
	if s.Now == nil {
		return time.Now().UTC()
	}
	return s.Now().UTC()
}

func stringPointer(value string) *string {
	return &value
}
