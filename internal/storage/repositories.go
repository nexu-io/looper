package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type sqliteQuerier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

type Repositories struct {
	Projects             *ProjectsRepository
	Loops                *LoopsRepository
	Runs                 *RunsRepository
	AgentExecutions      *AgentExecutionsRepository
	PullRequestSnapshots *PullRequestSnapshotsRepository
	Events               *EventsRepository
	Locks                *LocksRepository
	Queue                *QueueRepository
	Notifications        *NotificationsRepository
	Worktrees            *WorktreesRepository
	SweeperCases         *SweeperCasesRepository
	SweeperProposals     *SweeperProposalsRepository
	WebhookForwarders    *WebhookForwardersRepository
	WebhookTunnelHooks   *WebhookTunnelHooksRepository
}

func NewRepositories(q sqliteQuerier) *Repositories {
	return &Repositories{
		Projects:             &ProjectsRepository{q: q},
		Loops:                &LoopsRepository{q: q},
		Runs:                 &RunsRepository{q: q},
		AgentExecutions:      &AgentExecutionsRepository{q: q},
		PullRequestSnapshots: &PullRequestSnapshotsRepository{q: q},
		Events:               &EventsRepository{q: q},
		Locks:                &LocksRepository{q: q, now: time.Now},
		Queue:                &QueueRepository{q: q},
		Notifications:        &NotificationsRepository{q: q},
		Worktrees:            &WorktreesRepository{q: q},
		SweeperCases:         &SweeperCasesRepository{q: q},
		SweeperProposals:     &SweeperProposalsRepository{q: q},
		WebhookForwarders:    &WebhookForwardersRepository{q: q},
		WebhookTunnelHooks:   &WebhookTunnelHooksRepository{q: q},
	}
}

type ProjectRecord struct {
	ID           string
	Name         string
	RepoPath     string
	BaseBranch   *string
	Archived     bool
	MetadataJSON *string
	CreatedAt    string
	UpdatedAt    string
}

type LoopRecord struct {
	ID           string
	Seq          int64
	ProjectID    string
	Type         string
	TargetType   string
	TargetID     *string
	Repo         *string
	PRNumber     *int64
	Status       string
	ConfigJSON   *string
	MetadataJSON *string
	LastRunAt    *string
	NextRunAt    *string
	CreatedAt    string
	UpdatedAt    string
}

type RunRecord struct {
	ID                string
	LoopID            string
	Status            string
	CurrentStep       *string
	LastCompletedStep *string
	CheckpointJSON    *string
	Summary           *string
	ErrorMessage      *string
	StartedAt         string
	LastHeartbeatAt   *string
	EndedAt           *string
	CreatedAt         string
	UpdatedAt         string
}

type AgentExecutionRecord struct {
	ID                 string
	ProjectID          *string
	LoopID             *string
	RunID              *string
	Vendor             string
	Status             string
	PID                *int64
	CommandJSON        *string
	CWD                *string
	Summary            *string
	ParseStatus        *string
	CompletionSignal   *string
	HeartbeatCount     int64
	LastHeartbeatAt    *string
	OutputJSON         *string
	ErrorMessage       *string
	NativeSessionID    *string
	NativeResumeMode   *string
	NativeResumeStatus *string
	NativeResumeError  *string
	StartedAt          string
	EndedAt            *string
	MetadataJSON       *string
	CreatedAt          string
	UpdatedAt          string
}

type PullRequestSnapshotRecord struct {
	ID                    string
	ProjectID             string
	Repo                  string
	PRNumber              int64
	HeadSHA               string
	BaseSHA               *string
	Title                 *string
	Body                  *string
	Author                *string
	DiffRef               *string
	ChecksSummary         *string
	UnresolvedThreadCount *int64
	ReviewState           *string
	PayloadJSON           *string
	CapturedAt            string
	CreatedAt             string
}

type EventLogRecord struct {
	ID               string
	EventType        string
	ProjectID        *string
	LoopID           *string
	RunID            *string
	EntityType       *string
	EntityID         *string
	CorrelationID    *string
	CausationID      *string
	ActorType        *string
	ActorID          *string
	ActorDisplayName *string
	PayloadJSON      string
	CreatedAt        string
}

type LockRecord struct {
	Key       string
	Owner     string
	Reason    *string
	ExpiresAt string
	CreatedAt string
	UpdatedAt string
}

type QueueItemRecord struct {
	ID            string
	ProjectID     *string
	LoopID        *string
	Type          string
	TargetType    string
	TargetID      string
	Repo          *string
	PRNumber      *int64
	DedupeKey     string
	Priority      int64
	Status        string
	AvailableAt   string
	Attempts      int64
	MaxAttempts   int64
	ClaimedBy     *string
	ClaimedAt     *string
	StartedAt     *string
	FinishedAt    *string
	LockKey       *string
	PayloadJSON   *string
	LastError     *string
	LastErrorKind *string
	CreatedAt     string
	UpdatedAt     string
}

type SweeperCaseRecord struct {
	ID                     string
	ProjectID              string
	Repo                   string
	TargetType             string
	TargetNumber           int64
	Status                 string
	CurrentPhase           string
	CurrentCategory        *string
	CurrentConfidenceScore *int64
	WarningCommentID       *int64
	WarningMarkerUUID      *string
	LastProposalID         *string
	LastFingerprintJSON    *string
	LastHumanActivityAt    *string
	WarnedAt               *string
	CloseDueAt             *string
	TerminalOutcome        *string
	TerminalAt             *string
	CreatedAt              string
	UpdatedAt              string
}

type SweeperProposalRecord struct {
	ID               string
	CaseID           string
	ProjectID        string
	Repo             string
	TargetType       string
	TargetNumber     int64
	SchemaVersion    int64
	ProposerKind     string
	FactBundleJSON   string
	FingerprintJSON  string
	ProposalJSON     string
	RawResultJSON    *string
	Decision         string
	Category         string
	ConfidenceScore  int64
	Summary          *string
	Rationale        *string
	MarkerUUID       *string
	ValidationStatus *string
	ValidationError  *string
	ApplyStatus      *string
	ApplySummary     *string
	ApplyError       *string
	AppliedAt        *string
	CreatedAt        string
}

type QueueStats struct {
	TotalQueued                      int64
	EligibleQueued                   int64
	BlockedByTerminalOrPausedLoop    int64
	BlockedByLockKey                 int64
	BlockedByReviewerFixerDependency int64
	ScheduledForFuture               int64
	StaleQueued                      int64
}

type QueueMarkRetryInput struct {
	ID           string
	AvailableAt  string
	Attempts     int64
	ErrorMessage *string
	ErrorKind    string
	UpdatedAt    string
}

type QueueFailInput struct {
	ID           string
	Attempts     int64
	FinishedAt   string
	ErrorMessage *string
	ErrorKind    string
	UpdatedAt    string
}

type NotificationRecord struct {
	ID           string
	ProjectID    *string
	LoopID       *string
	RunID        *string
	EntityType   *string
	EntityID     *string
	Channel      string
	Level        string
	Title        string
	Subtitle     *string
	Body         string
	Status       string
	DedupeKey    *string
	ErrorMessage *string
	PayloadJSON  *string
	SentAt       *string
	CreatedAt    string
	UpdatedAt    string
}

type WorktreeRecord struct {
	ID           string
	ProjectID    string
	RepoPath     string
	WorktreePath string
	Branch       string
	BaseBranch   *string
	Status       string
	HeadSHA      *string
	MetadataJSON *string
	CreatedAt    string
	UpdatedAt    string
	CleanedAt    *string
}

type NotificationsRepository struct{ q sqliteQuerier }

func (r *NotificationsRepository) Upsert(ctx context.Context, record NotificationRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO notifications (id, project_id, loop_id, run_id, entity_type, entity_id, channel, level, title, subtitle, body, status, dedupe_key, error_message, payload_json, sent_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			run_id=excluded.run_id,
			entity_type=excluded.entity_type,
			entity_id=excluded.entity_id,
			channel=excluded.channel,
			level=excluded.level,
			title=excluded.title,
			subtitle=excluded.subtitle,
			body=excluded.body,
			status=excluded.status,
			dedupe_key=excluded.dedupe_key,
			error_message=excluded.error_message,
			payload_json=excluded.payload_json,
			sent_at=excluded.sent_at,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.LoopID, record.RunID, record.EntityType, record.EntityID, record.Channel, record.Level, record.Title, record.Subtitle, record.Body, record.Status, record.DedupeKey, record.ErrorMessage, record.PayloadJSON, record.SentAt, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert notification: %w", err)
	}

	return nil
}

func (r *NotificationsRepository) GetByID(ctx context.Context, id string) (*NotificationRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM notifications WHERE id = ?`, id)
	record, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get notification by id: %w", err)
	}

	return &record, nil
}

func (r *NotificationsRepository) List(ctx context.Context, limit int64) ([]NotificationRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `SELECT * FROM notifications ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()

	return scanNotifications(rows)
}

func (r *NotificationsRepository) GetLatestByDedupe(ctx context.Context, channel, dedupeKey string) (*NotificationRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM notifications WHERE channel = ? AND dedupe_key = ? ORDER BY created_at DESC LIMIT 1`, channel, dedupeKey)
	record, err := scanNotification(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest notification by dedupe: %w", err)
	}

	return &record, nil
}

type EventsRepository struct{ q sqliteQuerier }

func (r *EventsRepository) Append(ctx context.Context, record EventLogRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO event_logs (
			id, event_type, project_id, loop_id, run_id, entity_type, entity_id,
			correlation_id, causation_id, actor_type, actor_id, actor_display_name,
			payload_json, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.EventType, record.ProjectID, record.LoopID, record.RunID, record.EntityType, record.EntityID, record.CorrelationID, record.CausationID, record.ActorType, record.ActorID, record.ActorDisplayName, record.PayloadJSON, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("append event log: %w", err)
	}

	return nil
}

func (r *EventsRepository) List(ctx context.Context, limit int64) ([]EventLogRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `SELECT * FROM event_logs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list event logs: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

func (r *EventsRepository) ListSince(ctx context.Context, sinceISO string) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM event_logs WHERE created_at >= ? ORDER BY created_at DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list event logs since: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

func (r *EventsRepository) ListByEntity(ctx context.Context, entityType, entityID string) ([]EventLogRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM event_logs WHERE entity_type = ? AND entity_id = ? ORDER BY created_at ASC`, entityType, entityID)
	if err != nil {
		return nil, fmt.Errorf("list event logs by entity: %w", err)
	}
	defer rows.Close()

	return scanEventLogs(rows)
}

type ProjectsRepository struct{ q sqliteQuerier }

func (r *ProjectsRepository) Upsert(ctx context.Context, record ProjectRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO projects (id, name, repo_path, base_branch, archived, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name=excluded.name,
			repo_path=excluded.repo_path,
			base_branch=excluded.base_branch,
			archived=excluded.archived,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at
	`, record.ID, record.Name, record.RepoPath, record.BaseBranch, boolToInt(record.Archived), record.MetadataJSON, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert project: %w", err)
	}

	return nil
}

func (r *ProjectsRepository) GetByID(ctx context.Context, id string) (*ProjectRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM projects WHERE id = ?`, id)
	record, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get project by id: %w", err)
	}

	return &record, nil
}

func (r *ProjectsRepository) List(ctx context.Context) ([]ProjectRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

func (r *ProjectsRepository) Delete(ctx context.Context, id string) (bool, error) {
	result, err := r.q.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete project: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("delete project rows affected: %w", err)
	}

	return rowsAffected > 0, nil
}

type LoopsRepository struct{ q sqliteQuerier }

func (r *LoopsRepository) Upsert(ctx context.Context, record LoopRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO loops (id, seq, project_id, type, target_type, target_id, repo, pr_number, status, config_json, metadata_json, last_run_at, next_run_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			seq=excluded.seq,
			project_id=excluded.project_id,
			type=excluded.type,
			target_type=excluded.target_type,
			target_id=excluded.target_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			status=excluded.status,
			config_json=excluded.config_json,
			metadata_json=excluded.metadata_json,
			last_run_at=excluded.last_run_at,
			next_run_at=excluded.next_run_at,
			updated_at=excluded.updated_at
	`, record.ID, record.Seq, record.ProjectID, record.Type, record.TargetType, record.TargetID, record.Repo, record.PRNumber, record.Status, record.ConfigJSON, record.MetadataJSON, record.LastRunAt, record.NextRunAt, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert loop: %w", err)
	}

	_, err = r.q.ExecContext(ctx, `
		INSERT INTO counters (name, value)
		VALUES ('loop_seq', ?)
		ON CONFLICT(name) DO UPDATE SET value =
			CASE WHEN excluded.value > counters.value THEN excluded.value ELSE counters.value END
	`, record.Seq)
	if err != nil {
		return fmt.Errorf("upsert loop counter: %w", err)
	}

	return nil
}

func (r *LoopsRepository) GetByID(ctx context.Context, id string) (*LoopRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM loops WHERE id = ?`, id)
	record, err := scanLoop(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get loop by id: %w", err)
	}

	return &record, nil
}

func (r *LoopsRepository) GetBySeq(ctx context.Context, seq int64) (*LoopRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM loops WHERE seq = ?`, seq)
	record, err := scanLoop(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get loop by seq: %w", err)
	}

	return &record, nil
}

func (r *LoopsRepository) AllocateSeq(ctx context.Context) (int64, error) {
	var existing int64
	err := r.q.QueryRowContext(ctx, `SELECT value FROM counters WHERE name = 'loop_seq'`).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read loop counter: %w", err)
	}

	if errors.Is(err, sql.ErrNoRows) {
		var currentValue int64
		if maxErr := r.q.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) AS value FROM loops`).Scan(&currentValue); maxErr != nil {
			return 0, fmt.Errorf("read max loop seq: %w", maxErr)
		}
		if _, insertErr := r.q.ExecContext(ctx, `INSERT INTO counters (name, value) VALUES ('loop_seq', ?)`, currentValue); insertErr != nil {
			return 0, fmt.Errorf("seed loop counter: %w", insertErr)
		}
	}

	var next int64
	if err := r.q.QueryRowContext(ctx, `UPDATE counters SET value = value + 1 WHERE name = 'loop_seq' RETURNING value`).Scan(&next); err != nil {
		return 0, fmt.Errorf("allocate loop seq: %w", err)
	}

	return next, nil
}

func (r *LoopsRepository) List(ctx context.Context) ([]LoopRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM loops ORDER BY updated_at DESC, seq DESC`)
	if err != nil {
		return nil, fmt.Errorf("list loops: %w", err)
	}
	defer rows.Close()

	return scanLoops(rows)
}

type RunsRepository struct{ q sqliteQuerier }

type AgentExecutionsRepository struct{ q sqliteQuerier }

type SweeperCasesRepository struct{ q sqliteQuerier }

type SweeperProposalsRepository struct{ q sqliteQuerier }

const agentExecutionColumns = `id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd, summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at, output_json, error_message, native_session_id, native_resume_mode, native_resume_status, native_resume_error, started_at, ended_at, metadata_json, created_at, updated_at`

func (r *RunsRepository) Upsert(ctx context.Context, record RunRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO runs (id, loop_id, status, current_step, last_completed_step, checkpoint_json, summary, error_message, started_at, last_heartbeat_at, ended_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status=excluded.status,
			current_step=excluded.current_step,
			last_completed_step=excluded.last_completed_step,
			checkpoint_json=excluded.checkpoint_json,
			summary=excluded.summary,
			error_message=excluded.error_message,
			started_at=excluded.started_at,
			last_heartbeat_at=excluded.last_heartbeat_at,
			ended_at=excluded.ended_at,
			updated_at=excluded.updated_at
	`, record.ID, record.LoopID, record.Status, record.CurrentStep, record.LastCompletedStep, record.CheckpointJSON, record.Summary, record.ErrorMessage, record.StartedAt, record.LastHeartbeatAt, record.EndedAt, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}

	return nil
}

func (r *RunsRepository) GetByID(ctx context.Context, id string) (*RunRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM runs WHERE id = ?`, id)
	record, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get run by id: %w", err)
	}

	return &record, nil
}

func (r *RunsRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*RunRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM runs WHERE loop_id = ? ORDER BY started_at DESC, created_at DESC LIMIT 1`, loopID)
	record, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest run by loop id: %w", err)
	}

	return &record, nil
}

func (r *RunsRepository) HasRunningByLoopID(ctx context.Context, loopID string) (bool, error) {
	var count int64
	if err := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE loop_id = ? AND status = 'running'`, loopID).Scan(&count); err != nil {
		return false, fmt.Errorf("check running run by loop id: %w", err)
	}

	return count > 0, nil
}

func (r *RunsRepository) List(ctx context.Context) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) ListSince(ctx context.Context, sinceISO string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM runs WHERE started_at >= ? ORDER BY started_at DESC, id DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list runs since: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) ListByStatus(ctx context.Context, status string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM runs WHERE status = ? ORDER BY started_at DESC, id DESC`, status)
	if err != nil {
		return nil, fmt.Errorf("list runs by status: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *RunsRepository) ListByLoop(ctx context.Context, loopID string) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM runs WHERE loop_id = ? ORDER BY started_at DESC`, loopID)
	if err != nil {
		return nil, fmt.Errorf("list runs by loop: %w", err)
	}
	defer rows.Close()

	return scanRuns(rows)
}

func (r *AgentExecutionsRepository) Upsert(ctx context.Context, record AgentExecutionRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO agent_executions (id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd, summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at, output_json, error_message, native_session_id, native_resume_mode, native_resume_status, native_resume_error, started_at, ended_at, metadata_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			run_id=excluded.run_id,
			vendor=excluded.vendor,
			status=excluded.status,
			pid=excluded.pid,
			command_json=excluded.command_json,
			cwd=excluded.cwd,
			summary=excluded.summary,
			parse_status=excluded.parse_status,
			completion_signal=excluded.completion_signal,
			heartbeat_count=excluded.heartbeat_count,
			last_heartbeat_at=excluded.last_heartbeat_at,
			output_json=excluded.output_json,
			error_message=excluded.error_message,
			native_session_id=excluded.native_session_id,
			native_resume_mode=excluded.native_resume_mode,
			native_resume_status=excluded.native_resume_status,
			native_resume_error=excluded.native_resume_error,
			started_at=excluded.started_at,
			ended_at=excluded.ended_at,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.LoopID, record.RunID, record.Vendor, record.Status, record.PID, record.CommandJSON, record.CWD, record.Summary, record.ParseStatus, record.CompletionSignal, record.HeartbeatCount, record.LastHeartbeatAt, record.OutputJSON, record.ErrorMessage, record.NativeSessionID, record.NativeResumeMode, record.NativeResumeStatus, record.NativeResumeError, record.StartedAt, record.EndedAt, record.MetadataJSON, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert agent execution: %w", err)
	}

	return nil
}

func (r *SweeperCasesRepository) Upsert(ctx context.Context, record SweeperCaseRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO sweeper_cases (
			id, project_id, repo, target_type, target_number, status,
			current_phase, current_category, current_confidence_score,
			warning_comment_id, warning_marker_uuid, last_proposal_id,
			last_fingerprint_json, last_human_activity_at, warned_at,
			close_due_at, terminal_outcome, terminal_at, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			repo=excluded.repo,
			target_type=excluded.target_type,
			target_number=excluded.target_number,
			status=excluded.status,
			current_phase=excluded.current_phase,
			current_category=excluded.current_category,
			current_confidence_score=excluded.current_confidence_score,
			warning_comment_id=excluded.warning_comment_id,
			warning_marker_uuid=excluded.warning_marker_uuid,
			last_proposal_id=excluded.last_proposal_id,
			last_fingerprint_json=excluded.last_fingerprint_json,
			last_human_activity_at=excluded.last_human_activity_at,
			warned_at=excluded.warned_at,
			close_due_at=excluded.close_due_at,
			terminal_outcome=excluded.terminal_outcome,
			terminal_at=excluded.terminal_at,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.Repo, record.TargetType, record.TargetNumber, record.Status, record.CurrentPhase, record.CurrentCategory, record.CurrentConfidenceScore, record.WarningCommentID, record.WarningMarkerUUID, record.LastProposalID, record.LastFingerprintJSON, record.LastHumanActivityAt, record.WarnedAt, record.CloseDueAt, record.TerminalOutcome, record.TerminalAt, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert sweeper case: %w", err)
	}

	return nil
}

func (r *SweeperCasesRepository) GetByID(ctx context.Context, id string) (*SweeperCaseRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM sweeper_cases WHERE id = ?`, id)
	record, err := scanSweeperCase(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get sweeper case by id: %w", err)
	}

	return &record, nil
}

func (r *SweeperCasesRepository) GetByProjectRepoTarget(ctx context.Context, projectID, repo, targetType string, targetNumber int64) (*SweeperCaseRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT *
		FROM sweeper_cases
		WHERE project_id = ? AND repo = ? AND target_type = ? AND target_number = ?
		LIMIT 1
	`, projectID, repo, targetType, targetNumber)
	record, err := scanSweeperCase(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get sweeper case by target: %w", err)
	}

	return &record, nil
}

func (r *SweeperCasesRepository) ListByProjectRepoPhase(ctx context.Context, projectID, repo, phase string) ([]SweeperCaseRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		SELECT *
		FROM sweeper_cases
		WHERE project_id = ? AND repo = ? AND current_phase = ?
		ORDER BY updated_at DESC, id DESC
	`, projectID, repo, phase)
	if err != nil {
		return nil, fmt.Errorf("list sweeper cases by phase: %w", err)
	}
	defer rows.Close()

	return scanSweeperCases(rows)
}

func (r *SweeperCasesRepository) ListByProjectRepo(ctx context.Context, projectID, repo string, limit int) ([]SweeperCaseRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT *
		FROM sweeper_cases
		WHERE project_id = ? AND repo = ?
		ORDER BY updated_at DESC, id DESC
		LIMIT ?
	`, projectID, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("list sweeper cases by repo: %w", err)
	}
	defer rows.Close()

	return scanSweeperCases(rows)
}

func (r *SweeperCasesRepository) ListByProjectRepoStatus(ctx context.Context, projectID, repo, status string) ([]SweeperCaseRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		SELECT *
		FROM sweeper_cases
		WHERE project_id = ? AND repo = ? AND status = ?
		ORDER BY updated_at DESC, id DESC
	`, projectID, repo, status)
	if err != nil {
		return nil, fmt.Errorf("list sweeper cases by status: %w", err)
	}
	defer rows.Close()

	return scanSweeperCases(rows)
}

func (r *SweeperProposalsRepository) Insert(ctx context.Context, record SweeperProposalRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO sweeper_proposals (
			id, case_id, project_id, repo, target_type, target_number,
			schema_version, proposer_kind, fact_bundle_json, fingerprint_json,
			proposal_json, raw_result_json, decision, category, confidence_score, summary,
			rationale, marker_uuid, validation_status, validation_error,
			apply_status, apply_summary, apply_error, applied_at, created_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.CaseID, record.ProjectID, record.Repo, record.TargetType, record.TargetNumber, record.SchemaVersion, record.ProposerKind, record.FactBundleJSON, record.FingerprintJSON, record.ProposalJSON, record.RawResultJSON, record.Decision, record.Category, record.ConfidenceScore, record.Summary, record.Rationale, record.MarkerUUID, record.ValidationStatus, record.ValidationError, record.ApplyStatus, record.ApplySummary, record.ApplyError, record.AppliedAt, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert sweeper proposal: %w", err)
	}

	return nil
}

func (r *SweeperProposalsRepository) GetByID(ctx context.Context, id string) (*SweeperProposalRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT id, case_id, project_id, repo, target_type, target_number,
			schema_version, proposer_kind, fact_bundle_json, fingerprint_json,
			proposal_json, raw_result_json, decision, category, confidence_score,
			summary, rationale, marker_uuid, validation_status, validation_error,
			apply_status, apply_summary, apply_error, applied_at, created_at
		FROM sweeper_proposals
		WHERE id = ?
	`, id)
	record, err := scanSweeperProposal(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get sweeper proposal by id: %w", err)
	}

	return &record, nil
}

func (r *SweeperProposalsRepository) GetLatestByCaseID(ctx context.Context, caseID string) (*SweeperProposalRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT id, case_id, project_id, repo, target_type, target_number,
			schema_version, proposer_kind, fact_bundle_json, fingerprint_json,
			proposal_json, raw_result_json, decision, category, confidence_score,
			summary, rationale, marker_uuid, validation_status, validation_error,
			apply_status, apply_summary, apply_error, applied_at, created_at
		FROM sweeper_proposals
		WHERE case_id = ?
		ORDER BY created_at DESC, id DESC
		LIMIT 1
	`, caseID)
	record, err := scanSweeperProposal(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest sweeper proposal by case: %w", err)
	}

	return &record, nil
}

func (r *SweeperProposalsRepository) ListByCaseID(ctx context.Context, caseID string) ([]SweeperProposalRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		SELECT id, case_id, project_id, repo, target_type, target_number,
			schema_version, proposer_kind, fact_bundle_json, fingerprint_json,
			proposal_json, raw_result_json, decision, category, confidence_score,
			summary, rationale, marker_uuid, validation_status, validation_error,
			apply_status, apply_summary, apply_error, applied_at, created_at
		FROM sweeper_proposals
		WHERE case_id = ?
		ORDER BY created_at DESC, id DESC
	`, caseID)
	if err != nil {
		return nil, fmt.Errorf("list sweeper proposals by case: %w", err)
	}
	defer rows.Close()

	return scanSweeperProposals(rows)
}

func (r *SweeperProposalsRepository) ListByProjectRepo(ctx context.Context, projectID, repo string, limit int) ([]SweeperProposalRecord, error) {
	if limit <= 0 {
		limit = 1000
	}
	rows, err := r.q.QueryContext(ctx, `
		SELECT id, case_id, project_id, repo, target_type, target_number,
			schema_version, proposer_kind, fact_bundle_json, fingerprint_json,
			proposal_json, raw_result_json, decision, category, confidence_score,
			summary, rationale, marker_uuid, validation_status, validation_error,
			apply_status, apply_summary, apply_error, applied_at, created_at
		FROM sweeper_proposals
		WHERE project_id = ? AND repo = ?
		ORDER BY created_at DESC, id DESC
		LIMIT ?
	`, projectID, repo, limit)
	if err != nil {
		return nil, fmt.Errorf("list sweeper proposals by repo: %w", err)
	}
	defer rows.Close()

	return scanSweeperProposals(rows)
}

func (r *SweeperProposalsRepository) UpdateApplyReceipt(ctx context.Context, id string, applyStatus string, applySummary *string, applyError *string, appliedAt *string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE sweeper_proposals
		SET apply_status = ?,
			apply_summary = ?,
			apply_error = ?,
			applied_at = ?
		WHERE id = ?
	`, applyStatus, applySummary, applyError, appliedAt, id)
	if err != nil {
		return fmt.Errorf("update sweeper proposal apply receipt: %w", err)
	}

	return nil
}

func (r *SweeperProposalsRepository) CountAppliedByRepoAndDecisionSince(ctx context.Context, projectID, repo, decision, sinceISO string) (int64, error) {
	var count int64
	err := r.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sweeper_proposals
		WHERE project_id = ?
			AND repo = ?
			AND decision = ?
			AND applied_at IS NOT NULL
			AND applied_at >= ?
			AND apply_status = CASE
				WHEN ? = 'warn' THEN 'completed_warned'
				WHEN ? = 'close' THEN 'completed_closed'
				WHEN ? = 'cancel' THEN 'completed_cancelled'
				WHEN ? = 'quarantine' THEN 'completed_quarantined'
				ELSE apply_status
			END
	`, projectID, repo, decision, sinceISO, decision, decision, decision, decision).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count applied sweeper proposals by repo and decision: %w", err)
	}
	return count, nil
}

func (r *SweeperProposalsRepository) CountInflightByRepoAndDecision(ctx context.Context, projectID, repo, decision string) (int64, error) {
	var count int64
	err := r.q.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM sweeper_proposals p
		WHERE p.project_id = ?
			AND p.repo = ?
			AND p.decision = ?
			AND NOT EXISTS (
				SELECT 1
				FROM sweeper_proposals newer
				WHERE newer.case_id = p.case_id
					AND (newer.created_at > p.created_at OR (newer.created_at = p.created_at AND newer.id > p.id))
			)
			AND (p.apply_status IS NULL OR (
				p.apply_status NOT LIKE 'completed_%'
				AND p.apply_status NOT LIKE 'skipped_%'
				AND p.apply_status NOT LIKE 'failed_%'
			))
	`, projectID, repo, decision).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count inflight sweeper proposals by repo and decision: %w", err)
	}
	return count, nil
}

func (r *AgentExecutionsRepository) GetByID(ctx context.Context, id string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE id = ?`, id)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get agent execution by id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestByRunID(ctx context.Context, runID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE run_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, runID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest agent execution by run id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestActiveByRunID(ctx context.Context, runID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE run_id = ? AND status IN ('running', 'cancelling') ORDER BY started_at DESC, id DESC LIMIT 1`, runID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest active agent execution by run id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*AgentExecutionRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE loop_id = ? ORDER BY started_at DESC, id DESC LIMIT 1`, loopID)
	record, err := scanAgentExecution(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest agent execution by loop id: %w", err)
	}

	return &record, nil
}

func (r *AgentExecutionsRepository) ListActive(ctx context.Context) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE status IN ('running', 'cancelling') ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list active agent executions: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

func (r *AgentExecutionsRepository) List(ctx context.Context) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions ORDER BY started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("list agent executions: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

func (r *AgentExecutionsRepository) ListSince(ctx context.Context, sinceISO string) ([]AgentExecutionRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT `+agentExecutionColumns+` FROM agent_executions WHERE started_at >= ? ORDER BY started_at DESC, id DESC`, sinceISO)
	if err != nil {
		return nil, fmt.Errorf("list agent executions since: %w", err)
	}
	defer rows.Close()

	return scanAgentExecutions(rows)
}

type PullRequestSnapshotsRepository struct{ q sqliteQuerier }

func (r *PullRequestSnapshotsRepository) Upsert(ctx context.Context, record PullRequestSnapshotRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO pull_request_snapshots (id, project_id, repo, pr_number, head_sha, base_sha, title, body, author, diff_ref, checks_summary, unresolved_thread_count, review_state, payload_json, captured_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			head_sha=excluded.head_sha,
			base_sha=excluded.base_sha,
			title=excluded.title,
			body=excluded.body,
			author=excluded.author,
			diff_ref=excluded.diff_ref,
			checks_summary=excluded.checks_summary,
			unresolved_thread_count=excluded.unresolved_thread_count,
			review_state=excluded.review_state,
			payload_json=excluded.payload_json,
			captured_at=excluded.captured_at
	`, record.ID, record.ProjectID, record.Repo, record.PRNumber, record.HeadSHA, record.BaseSHA, record.Title, record.Body, record.Author, record.DiffRef, record.ChecksSummary, record.UnresolvedThreadCount, record.ReviewState, record.PayloadJSON, record.CapturedAt, record.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert pull request snapshot: %w", err)
	}

	return nil
}

func (r *PullRequestSnapshotsRepository) List(ctx context.Context) ([]PullRequestSnapshotRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM pull_request_snapshots ORDER BY captured_at DESC, created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list pull request snapshots: %w", err)
	}
	defer rows.Close()

	return scanPullRequestSnapshots(rows)
}

func (r *PullRequestSnapshotsRepository) GetLatest(ctx context.Context, repo string, prNumber int64) (*PullRequestSnapshotRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM pull_request_snapshots WHERE repo = ? AND pr_number = ? ORDER BY captured_at DESC LIMIT 1`, repo, prNumber)
	record, err := scanPullRequestSnapshot(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest pull request snapshot: %w", err)
	}

	return &record, nil
}

func (r *PullRequestSnapshotsRepository) GetLatestByProject(ctx context.Context, projectID, repo string, prNumber int64) (*PullRequestSnapshotRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM pull_request_snapshots WHERE project_id = ? AND repo = ? AND pr_number = ? ORDER BY captured_at DESC, created_at DESC LIMIT 1`, projectID, repo, prNumber)
	record, err := scanPullRequestSnapshot(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest pull request snapshot by project: %w", err)
	}

	return &record, nil
}

type LocksRepository struct {
	q   sqliteQuerier
	now func() time.Time
}

func (r *LocksRepository) SetNow(now func() time.Time) {
	if now == nil {
		r.now = time.Now
		return
	}

	r.now = now
}

func (r *LocksRepository) Acquire(ctx context.Context, record LockRecord) (bool, error) {
	nowISO := r.now().UTC().Format(javaScriptISOStringLayout)
	result, err := r.q.ExecContext(ctx, `
		INSERT INTO locks (key, owner, reason, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			owner=excluded.owner,
			reason=excluded.reason,
			expires_at=excluded.expires_at,
			updated_at=excluded.updated_at
		WHERE locks.expires_at <= ?
	`, record.Key, record.Owner, record.Reason, record.ExpiresAt, record.CreatedAt, record.UpdatedAt, nowISO)
	if err != nil {
		return false, fmt.Errorf("acquire lock: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read acquire lock rows affected: %w", err)
	}

	return affected > 0, nil
}

func (r *LocksRepository) Release(ctx context.Context, key string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM locks WHERE key = ?`, key)
	if err != nil {
		return fmt.Errorf("release lock: %w", err)
	}

	return nil
}

func (r *LocksRepository) Get(ctx context.Context, key string) (*LockRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM locks WHERE key = ?`, key)
	record, err := scanLock(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get lock: %w", err)
	}

	return &record, nil
}

func (r *LocksRepository) Refresh(ctx context.Context, record LockRecord) (bool, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE locks
		SET reason = ?, expires_at = ?, updated_at = ?
		WHERE key = ? AND owner = ?
	`, record.Reason, record.ExpiresAt, record.UpdatedAt, record.Key, record.Owner)
	if err != nil {
		return false, fmt.Errorf("refresh lock: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("read refresh lock rows affected: %w", err)
	}

	return affected > 0, nil
}

func (r *LocksRepository) ListExpired(ctx context.Context, nowISO string) ([]LockRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM locks WHERE expires_at <= ? ORDER BY expires_at ASC`, nowISO)
	if err != nil {
		return nil, fmt.Errorf("list expired locks: %w", err)
	}
	defer rows.Close()

	return scanLocks(rows)
}

type WorktreesRepository struct{ q sqliteQuerier }

type QueueRepository struct{ q sqliteQuerier }

func (r *QueueRepository) CreateOrGetActiveByDedupe(ctx context.Context, record QueueItemRecord) (QueueItemRecord, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		persisted, didPersist, err := r.UpsertActiveByDedupeOrGetExisting(ctx, record)
		if err == nil {
			return persisted, didPersist, nil
		}
		if !shouldRecoverActiveDedupeConflict(record, err) {
			return QueueItemRecord{}, false, err
		}
	}
	return QueueItemRecord{}, false, fmt.Errorf("upsert queue item: active dedupe conflict for %s", record.DedupeKey)
}

func (r *QueueRepository) UpsertActiveByDedupeOrGetExisting(ctx context.Context, record QueueItemRecord) (QueueItemRecord, bool, error) {
	err := r.Upsert(ctx, record)
	if err == nil {
		return record, true, nil
	}
	if !shouldRecoverActiveDedupeConflict(record, err) {
		return QueueItemRecord{}, false, err
	}
	existing, findErr := r.FindActiveByDedupe(ctx, record.DedupeKey)
	if findErr != nil {
		return QueueItemRecord{}, false, findErr
	}
	if existing != nil {
		return *existing, false, nil
	}
	return QueueItemRecord{}, false, err
}

func shouldRecoverActiveDedupeConflict(record QueueItemRecord, err error) bool {
	if !isQueueActiveDedupeConstraintError(err) {
		return false
	}
	if record.DedupeKey == "" {
		return false
	}
	return record.Type == "reviewer" || record.Type == "fixer"
}

func (r *QueueRepository) Upsert(ctx context.Context, record QueueItemRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO queue_items (
			id, project_id, loop_id, type, target_type, target_id, repo,
			pr_number, dedupe_key, priority, status, available_at, attempts,
			max_attempts, claimed_by, claimed_at, started_at, finished_at,
			lock_key, payload_json, last_error, last_error_kind, created_at,
			updated_at
		)
		VALUES (
			?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?
		)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			loop_id=excluded.loop_id,
			type=excluded.type,
			target_type=excluded.target_type,
			target_id=excluded.target_id,
			repo=excluded.repo,
			pr_number=excluded.pr_number,
			dedupe_key=excluded.dedupe_key,
			priority=excluded.priority,
			status=excluded.status,
			available_at=excluded.available_at,
			attempts=excluded.attempts,
			max_attempts=excluded.max_attempts,
			claimed_by=excluded.claimed_by,
			claimed_at=excluded.claimed_at,
			started_at=excluded.started_at,
			finished_at=excluded.finished_at,
			lock_key=excluded.lock_key,
			payload_json=excluded.payload_json,
			last_error=excluded.last_error,
			last_error_kind=excluded.last_error_kind,
			updated_at=excluded.updated_at
	`, record.ID, record.ProjectID, record.LoopID, record.Type, record.TargetType, record.TargetID, record.Repo, record.PRNumber, record.DedupeKey, record.Priority, record.Status, record.AvailableAt, record.Attempts, record.MaxAttempts, record.ClaimedBy, record.ClaimedAt, record.StartedAt, record.FinishedAt, record.LockKey, record.PayloadJSON, record.LastError, record.LastErrorKind, record.CreatedAt, record.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert queue item: %w", err)
	}

	return nil
}

func (r *QueueRepository) GetByID(ctx context.Context, id string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM queue_items WHERE id = ?`, id)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get queue item by id: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) GetLatestByLoopID(ctx context.Context, loopID string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT * FROM queue_items
		WHERE loop_id = ?
		ORDER BY updated_at DESC, created_at DESC, id DESC
		LIMIT 1
	`, loopID)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest queue item by loop: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) List(ctx context.Context) ([]QueueItemRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM queue_items ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) ListQueued(ctx context.Context, limit int64) ([]QueueItemRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, `
		SELECT *
		FROM queue_items
		WHERE status = 'queued'
		ORDER BY priority ASC, available_at ASC, created_at ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list queued queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) CountByStatus(ctx context.Context, status string) (int64, error) {
	row := r.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM queue_items WHERE status = ?`, status)
	var count int64
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count queue items by status: %w", err)
	}
	return count, nil
}

func (r *QueueRepository) FindActiveByDedupe(ctx context.Context, dedupeKey string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT * FROM queue_items
		WHERE dedupe_key = ? AND status IN ('queued', 'running')
		ORDER BY created_at DESC
		LIMIT 1
	`, dedupeKey)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active queue item by dedupe: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) FindActiveByLoopID(ctx context.Context, loopID string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT * FROM queue_items
		WHERE loop_id = ? AND status IN ('queued', 'running')
		ORDER BY created_at DESC
		LIMIT 1
	`, loopID)
	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("find active queue item by loop: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) ListScheduled(ctx context.Context, nowISO string, limit int64) ([]QueueItemRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := r.q.QueryContext(ctx, scheduledQueueQuery+` LIMIT ?`, nowISO, limit)
	if err != nil {
		return nil, fmt.Errorf("list scheduled queue items: %w", err)
	}
	defer rows.Close()

	return scanQueueItems(rows)
}

func (r *QueueRepository) Stats(ctx context.Context, nowISO string) (QueueStats, error) {
	stats := QueueStats{}
	queries := []struct {
		name string
		dest *int64
		sql  string
		now  bool
	}{
		{name: "total queued", dest: &stats.TotalQueued, sql: `SELECT COUNT(*) FROM queue_items WHERE status = 'queued'`},
		{name: "eligible queued", dest: &stats.EligibleQueued, sql: `SELECT COUNT(*) FROM (` + scheduledQueueBaseQuery + `)`, now: true},
		{name: "blocked by terminal or paused loop", dest: &stats.BlockedByTerminalOrPausedLoop, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			JOIN loops l ON l.id = qi.loop_id
			WHERE qi.status = 'queued'
				AND l.status IN ('paused', 'completed', 'failed', 'interrupted', 'terminated', 'stopped')
		`},
		{name: "blocked by lock key", dest: &stats.BlockedByLockKey, now: true, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			WHERE qi.status = 'queued'
				AND qi.available_at <= ?
				AND qi.lock_key IS NOT NULL
				AND EXISTS (
					SELECT 1
					FROM queue_items lock_blocker
					WHERE lock_blocker.lock_key = qi.lock_key
						AND lock_blocker.status = 'running'
						AND lock_blocker.id != qi.id
				)
		`},
		{name: "blocked by reviewer/fixer dependency", dest: &stats.BlockedByReviewerFixerDependency, now: true, sql: `
			SELECT COUNT(*)
			FROM queue_items qi
			WHERE qi.status = 'queued'
				AND qi.available_at <= ?
				AND qi.type = 'fixer'
				AND qi.repo IS NOT NULL
				AND qi.pr_number IS NOT NULL
				AND EXISTS (
					SELECT 1
					FROM queue_items blocker
					WHERE blocker.type = 'reviewer'
						AND blocker.repo = qi.repo
						AND blocker.pr_number = qi.pr_number
						AND blocker.status IN ('queued', 'running')
						AND blocker.id != qi.id
				)
		`},
		{name: "scheduled for future", dest: &stats.ScheduledForFuture, sql: `SELECT COUNT(*) FROM queue_items WHERE status = 'queued' AND available_at > ?`, now: true},
		{name: "stale queued", dest: &stats.StaleQueued, sql: staleQueuedCountQuery},
	}

	for _, query := range queries {
		args := []any{}
		if query.now {
			args = append(args, nowISO)
		}
		if err := r.q.QueryRowContext(ctx, query.sql, args...).Scan(query.dest); err != nil {
			return QueueStats{}, fmt.Errorf("count queue items %s: %w", query.name, err)
		}
	}
	return stats, nil
}

func (r *QueueRepository) CleanupStaleQueued(ctx context.Context, finishedAt string, reason string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = ?,
			last_error_kind = 'non_retryable',
			updated_at = ?
		WHERE id IN (`+staleQueuedIDsQuery+`)
	`, finishedAt, reason, finishedAt)
	if err != nil {
		return 0, fmt.Errorf("cleanup stale queued items: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cleanup stale queued items rows affected: %w", err)
	}
	return affected, nil
}

func (r *QueueRepository) ClaimNext(ctx context.Context, nowISO, claimedBy string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		WITH candidate AS (
			`+scheduledQueueQuery+`
			LIMIT 1
		)
		UPDATE queue_items
		SET status = 'running',
			claimed_by = ?,
			claimed_at = ?,
			started_at = COALESCE(started_at, ?),
			updated_at = ?
		WHERE id = (SELECT id FROM candidate)
			AND status = 'queued'
		RETURNING *
	`, nowISO, claimedBy, nowISO, nowISO, nowISO)

	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next queue item: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) ClaimNextOfType(ctx context.Context, nowISO, claimedBy, queueType string) (*QueueItemRecord, error) {
	row := r.q.QueryRowContext(ctx, `
		WITH candidate AS (
			`+scheduledQueueBaseQuery+`
			AND qi.type = ?
			`+scheduledQueueOrderBy+`
			LIMIT 1
		)
		UPDATE queue_items
		SET status = 'running',
			claimed_by = ?,
			claimed_at = ?,
			started_at = COALESCE(started_at, ?),
			updated_at = ?
		WHERE id = (SELECT id FROM candidate)
			AND status = 'queued'
		RETURNING *
	`, nowISO, queueType, claimedBy, nowISO, nowISO, nowISO)

	record, err := scanQueueItem(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("claim next queue item of type: %w", err)
	}

	return &record, nil
}

func (r *QueueRepository) Complete(ctx context.Context, id, finishedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'completed', finished_at = ?, updated_at = ?
		WHERE id = ?
	`, finishedAt, finishedAt, id)
	if err != nil {
		return fmt.Errorf("complete queue item: %w", err)
	}

	return nil
}

func (r *QueueRepository) UpdateLockKey(ctx context.Context, id, lockKey, updatedAt string) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET lock_key = ?, updated_at = ?
		WHERE id = ?
	`, lockKey, updatedAt, id)
	if err != nil {
		return fmt.Errorf("update queue item lock key: %w", err)
	}

	return nil
}

func (r *QueueRepository) MarkRetry(ctx context.Context, input QueueMarkRetryInput) error {
	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = ?,
			last_error = ?,
			last_error_kind = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			finished_at = NULL,
			updated_at = ?
		WHERE id = ?
	`, input.AvailableAt, input.Attempts, input.ErrorMessage, input.ErrorKind, input.UpdatedAt, input.ID)
	if err != nil {
		return fmt.Errorf("mark queue item for retry: %w", err)
	}

	return nil
}

func (r *QueueRepository) Fail(ctx context.Context, input QueueFailInput) error {
	terminalStatus := "failed"
	if input.ErrorKind == "manual_intervention" {
		terminalStatus = "manual_intervention"
	}

	_, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = ?,
			attempts = CASE WHEN ? > 0 THEN ? ELSE attempts END,
			finished_at = ?,
			last_error = ?,
			last_error_kind = ?,
			updated_at = ?
		WHERE id = ?
	`, terminalStatus, input.Attempts, input.Attempts, input.FinishedAt, input.ErrorMessage, input.ErrorKind, input.UpdatedAt, input.ID)
	if err != nil {
		return fmt.Errorf("fail queue item: %w", err)
	}

	return nil
}

func (r *QueueRepository) RequeueRunningByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			updated_at = ?
		WHERE loop_id = ? AND status = 'running'
	`, queuedAt, queuedAt, loopID)
	if err != nil {
		return 0, fmt.Errorf("requeue running queue items by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue queue items rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) RequeueLatestCancelledByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	queueID, err := r.findLatestCancelledQueueIDByLoop(ctx, loopID, true)
	if err != nil {
		return 0, err
	}
	if queueID == "" {
		queueID, err = r.findLatestCancelledQueueIDByLoop(ctx, loopID, false)
		if err != nil {
			return 0, err
		}
	}
	if queueID == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND status = 'cancelled'
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, queuedAt, queueID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue latest cancelled queue item by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue latest cancelled queue item rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) RequeueLatestFailedByLoop(ctx context.Context, loopID, queuedAt string) (int64, error) {
	queueID, err := r.findLatestQueueIDByLoopStatus(ctx, loopID, "failed")
	if err != nil {
		return 0, err
	}
	if queueID == "" {
		return 0, nil
	}

	return r.RequeueFailedByID(ctx, loopID, queueID, queuedAt)
}

func (r *QueueRepository) RequeueFailedByID(ctx context.Context, loopID, queueID, queuedAt string) (int64, error) {
	if loopID == "" || queueID == "" {
		return 0, nil
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = 0,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND loop_id = ? AND status = 'failed'
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, queuedAt, queueID, loopID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue failed queue item by id: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue failed queue item rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) RequeueFailedByIDWithAttempts(ctx context.Context, loopID, queueID, queuedAt string, attempts int64) (int64, error) {
	if loopID == "" || queueID == "" {
		return 0, nil
	}
	if attempts < 0 {
		attempts = 0
	}

	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'queued',
			available_at = ?,
			attempts = ?,
			claimed_by = NULL,
			claimed_at = NULL,
			started_at = NULL,
			finished_at = NULL,
			last_error = NULL,
			last_error_kind = NULL,
			updated_at = ?
		WHERE id = ? AND loop_id = ? AND status = 'failed'
			AND NOT EXISTS (
				SELECT 1 FROM queue_items
				WHERE loop_id = ? AND status IN ('queued', 'running') AND id != ?
			)
	`, queuedAt, attempts, queuedAt, queueID, loopID, loopID, queueID)
	if err != nil {
		return 0, fmt.Errorf("requeue failed queue item by id with attempts: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read requeue failed queue item rows affected: %w", err)
	}

	return affected, nil
}

func (r *QueueRepository) findLatestQueueIDByLoopStatus(ctx context.Context, loopID, status string) (string, error) {
	row := r.q.QueryRowContext(ctx, `
		SELECT id
		FROM queue_items
		WHERE loop_id = ? AND status = ?
		ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1
	`, loopID, status)
	var queueID string
	if err := row.Scan(&queueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get latest %s queue item by loop: %w", status, err)
	}
	return queueID, nil
}

func (r *QueueRepository) findLatestCancelledQueueIDByLoop(ctx context.Context, loopID string, onlyUnstarted bool) (string, error) {
	query := `
		SELECT id
		FROM queue_items
		WHERE loop_id = ? AND status = 'cancelled'
	`
	args := []any{loopID}
	if onlyUnstarted {
		query += ` AND started_at IS NULL`
	}
	query += ` ORDER BY updated_at DESC, created_at DESC, id DESC LIMIT 1`

	row := r.q.QueryRowContext(ctx, query, args...)
	var queueID string
	if err := row.Scan(&queueID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get latest cancelled queue item by loop: %w", err)
	}
	return queueID, nil
}

func (r *QueueRepository) CancelByLoop(ctx context.Context, loopID, finishedAt string, reason *string) (int64, error) {
	result, err := r.q.ExecContext(ctx, `
		UPDATE queue_items
		SET status = 'cancelled',
			finished_at = ?,
			last_error = COALESCE(?, last_error),
			updated_at = ?
		WHERE loop_id = ? AND status IN ('queued', 'running')
	`, finishedAt, reason, finishedAt, loopID)
	if err != nil {
		return 0, fmt.Errorf("cancel queue items by loop: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("read cancel queue items rows affected: %w", err)
	}

	return affected, nil
}

func (r *WorktreesRepository) Upsert(ctx context.Context, record WorktreeRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO worktrees (id, project_id, repo_path, worktree_path, branch, base_branch, status, head_sha, metadata_json, created_at, updated_at, cleaned_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_id=excluded.project_id,
			repo_path=excluded.repo_path,
			worktree_path=excluded.worktree_path,
			branch=excluded.branch,
			base_branch=excluded.base_branch,
			status=excluded.status,
			head_sha=excluded.head_sha,
			metadata_json=excluded.metadata_json,
			updated_at=excluded.updated_at,
			cleaned_at=excluded.cleaned_at
	`, record.ID, record.ProjectID, record.RepoPath, record.WorktreePath, record.Branch, record.BaseBranch, record.Status, record.HeadSHA, record.MetadataJSON, record.CreatedAt, record.UpdatedAt, record.CleanedAt)
	if err != nil {
		return fmt.Errorf("upsert worktree: %w", err)
	}

	return nil
}

func (r *WorktreesRepository) GetByID(ctx context.Context, id string) (*WorktreeRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM worktrees WHERE id = ?`, id)
	record, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get worktree by id: %w", err)
	}

	return &record, nil
}

func (r *WorktreesRepository) GetByBranch(ctx context.Context, projectID, branch string) (*WorktreeRecord, error) {
	row := r.q.QueryRowContext(ctx, `SELECT * FROM worktrees WHERE project_id = ? AND branch = ? LIMIT 1`, projectID, branch)
	record, err := scanWorktree(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get worktree by branch: %w", err)
	}

	return &record, nil
}

func (r *WorktreesRepository) ListByProject(ctx context.Context, projectID string) ([]WorktreeRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM worktrees WHERE project_id = ? ORDER BY updated_at DESC`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list worktrees by project: %w", err)
	}
	defer rows.Close()

	return scanWorktrees(rows)
}

func (r *WorktreesRepository) ListActive(ctx context.Context) ([]WorktreeRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM worktrees WHERE cleaned_at IS NULL ORDER BY updated_at ASC, created_at ASC`)
	if err != nil {
		return nil, fmt.Errorf("list active worktrees: %w", err)
	}
	defer rows.Close()

	return scanWorktrees(rows)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}

	return 0
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}

	stringValue := value.String
	return &stringValue
}

func nullableInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}

	intValue := value.Int64
	return &intValue
}

const scheduledQueueBaseQuery = `
	SELECT qi.*
	FROM queue_items qi
	LEFT JOIN loops l ON l.id = qi.loop_id
	WHERE qi.status = 'queued'
		AND qi.available_at <= ?
		AND COALESCE(l.status, 'queued') NOT IN ('paused', 'completed', 'failed', 'interrupted', 'terminated', 'stopped')
		AND (
			qi.lock_key IS NULL
			OR NOT EXISTS (
				SELECT 1
				FROM queue_items lock_blocker
				WHERE lock_blocker.lock_key = qi.lock_key
					AND lock_blocker.status = 'running'
					AND lock_blocker.id != qi.id
			)
		)
		AND (
			qi.type != 'fixer'
			OR qi.repo IS NULL
			OR qi.pr_number IS NULL
			OR NOT EXISTS (
				SELECT 1
				FROM queue_items blocker
				WHERE blocker.type = 'reviewer'
					AND blocker.repo = qi.repo
					AND blocker.pr_number = qi.pr_number
					AND blocker.status IN ('queued', 'running')
					AND blocker.id != qi.id
			)
		)
`

const scheduledQueueOrderBy = `
	ORDER BY qi.priority ASC, qi.available_at ASC, qi.created_at ASC
`

const scheduledQueueQuery = scheduledQueueBaseQuery + scheduledQueueOrderBy

const staleQueuedIDsQuery = `
	SELECT qi.id
	FROM queue_items qi
	JOIN loops l ON l.id = qi.loop_id
	WHERE qi.status = 'queued'
		AND l.status IN ('completed', 'failed', 'interrupted', 'terminated', 'stopped')
`

const staleQueuedCountQuery = `SELECT COUNT(*) FROM (` + staleQueuedIDsQuery + `)`

func scanProjects(rows *sql.Rows) ([]ProjectRecord, error) {
	records := make([]ProjectRecord, 0)
	for rows.Next() {
		record, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project rows: %w", err)
	}

	return records, nil
}

func scanProject(row interface{ Scan(...any) error }) (ProjectRecord, error) {
	var (
		record       ProjectRecord
		baseBranch   sql.NullString
		metadataJSON sql.NullString
		archived     int
	)

	err := row.Scan(&record.ID, &record.Name, &record.RepoPath, &baseBranch, &archived, &metadataJSON, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return ProjectRecord{}, err
	}
	record.BaseBranch = nullableString(baseBranch)
	record.Archived = archived == 1
	record.MetadataJSON = nullableString(metadataJSON)

	return record, nil
}

func scanLoops(rows *sql.Rows) ([]LoopRecord, error) {
	records := make([]LoopRecord, 0)
	for rows.Next() {
		record, err := scanLoop(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate loop rows: %w", err)
	}

	return records, nil
}

func scanLoop(row interface{ Scan(...any) error }) (LoopRecord, error) {
	var (
		record       LoopRecord
		targetID     sql.NullString
		repo         sql.NullString
		prNumber     sql.NullInt64
		configJSON   sql.NullString
		metadataJSON sql.NullString
		lastRunAt    sql.NullString
		nextRunAt    sql.NullString
	)

	err := row.Scan(&record.ID, &record.Seq, &record.ProjectID, &record.Type, &record.TargetType, &targetID, &repo, &prNumber, &record.Status, &configJSON, &metadataJSON, &lastRunAt, &nextRunAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return LoopRecord{}, err
	}
	record.TargetID = nullableString(targetID)
	record.Repo = nullableString(repo)
	record.PRNumber = nullableInt64(prNumber)
	record.ConfigJSON = nullableString(configJSON)
	record.MetadataJSON = nullableString(metadataJSON)
	record.LastRunAt = nullableString(lastRunAt)
	record.NextRunAt = nullableString(nextRunAt)

	return record, nil
}

func scanRuns(rows *sql.Rows) ([]RunRecord, error) {
	records := make([]RunRecord, 0)
	for rows.Next() {
		record, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate run rows: %w", err)
	}

	return records, nil
}

func scanAgentExecutions(rows *sql.Rows) ([]AgentExecutionRecord, error) {
	records := make([]AgentExecutionRecord, 0)
	for rows.Next() {
		record, err := scanAgentExecution(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate agent execution rows: %w", err)
	}

	return records, nil
}

func scanAgentExecution(row interface{ Scan(...any) error }) (AgentExecutionRecord, error) {
	var (
		record             AgentExecutionRecord
		projectID          sql.NullString
		loopID             sql.NullString
		runID              sql.NullString
		pid                sql.NullInt64
		commandJSON        sql.NullString
		cwd                sql.NullString
		summary            sql.NullString
		parseStatus        sql.NullString
		completionSignal   sql.NullString
		lastHeartbeatAt    sql.NullString
		outputJSON         sql.NullString
		errorMessage       sql.NullString
		nativeSessionID    sql.NullString
		nativeResumeMode   sql.NullString
		nativeResumeStatus sql.NullString
		nativeResumeError  sql.NullString
		endedAt            sql.NullString
		metadataJSON       sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&runID,
		&record.Vendor,
		&record.Status,
		&pid,
		&commandJSON,
		&cwd,
		&summary,
		&parseStatus,
		&completionSignal,
		&record.HeartbeatCount,
		&lastHeartbeatAt,
		&outputJSON,
		&errorMessage,
		&nativeSessionID,
		&nativeResumeMode,
		&nativeResumeStatus,
		&nativeResumeError,
		&record.StartedAt,
		&endedAt,
		&metadataJSON,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return AgentExecutionRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.PID = nullableInt64(pid)
	record.CommandJSON = nullableString(commandJSON)
	record.CWD = nullableString(cwd)
	record.Summary = nullableString(summary)
	record.ParseStatus = nullableString(parseStatus)
	record.CompletionSignal = nullableString(completionSignal)
	record.LastHeartbeatAt = nullableString(lastHeartbeatAt)
	record.OutputJSON = nullableString(outputJSON)
	record.ErrorMessage = nullableString(errorMessage)
	record.NativeSessionID = nullableString(nativeSessionID)
	record.NativeResumeMode = nullableString(nativeResumeMode)
	record.NativeResumeStatus = nullableString(nativeResumeStatus)
	record.NativeResumeError = nullableString(nativeResumeError)
	record.EndedAt = nullableString(endedAt)
	record.MetadataJSON = nullableString(metadataJSON)

	return record, nil
}

func scanRun(row interface{ Scan(...any) error }) (RunRecord, error) {
	var (
		record            RunRecord
		currentStep       sql.NullString
		lastCompletedStep sql.NullString
		checkpointJSON    sql.NullString
		summary           sql.NullString
		errorMessage      sql.NullString
		lastHeartbeatAt   sql.NullString
		endedAt           sql.NullString
	)

	err := row.Scan(&record.ID, &record.LoopID, &record.Status, &currentStep, &lastCompletedStep, &checkpointJSON, &summary, &errorMessage, &record.StartedAt, &lastHeartbeatAt, &endedAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return RunRecord{}, err
	}
	record.CurrentStep = nullableString(currentStep)
	record.LastCompletedStep = nullableString(lastCompletedStep)
	record.CheckpointJSON = nullableString(checkpointJSON)
	record.Summary = nullableString(summary)
	record.ErrorMessage = nullableString(errorMessage)
	record.LastHeartbeatAt = nullableString(lastHeartbeatAt)
	record.EndedAt = nullableString(endedAt)

	return record, nil
}

func scanPullRequestSnapshots(rows *sql.Rows) ([]PullRequestSnapshotRecord, error) {
	records := make([]PullRequestSnapshotRecord, 0)
	for rows.Next() {
		record, err := scanPullRequestSnapshot(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pull request snapshot rows: %w", err)
	}

	return records, nil
}

func scanPullRequestSnapshot(row interface{ Scan(...any) error }) (PullRequestSnapshotRecord, error) {
	var (
		record                PullRequestSnapshotRecord
		baseSHA               sql.NullString
		title                 sql.NullString
		body                  sql.NullString
		author                sql.NullString
		diffRef               sql.NullString
		checksSummary         sql.NullString
		unresolvedThreadCount sql.NullInt64
		reviewState           sql.NullString
		payloadJSON           sql.NullString
	)

	err := row.Scan(&record.ID, &record.ProjectID, &record.Repo, &record.PRNumber, &record.HeadSHA, &baseSHA, &title, &body, &author, &diffRef, &checksSummary, &unresolvedThreadCount, &reviewState, &payloadJSON, &record.CapturedAt, &record.CreatedAt)
	if err != nil {
		return PullRequestSnapshotRecord{}, err
	}
	record.BaseSHA = nullableString(baseSHA)
	record.Title = nullableString(title)
	record.Body = nullableString(body)
	record.Author = nullableString(author)
	record.DiffRef = nullableString(diffRef)
	record.ChecksSummary = nullableString(checksSummary)
	record.UnresolvedThreadCount = nullableInt64(unresolvedThreadCount)
	record.ReviewState = nullableString(reviewState)
	record.PayloadJSON = nullableString(payloadJSON)

	return record, nil
}

func scanEventLogs(rows *sql.Rows) ([]EventLogRecord, error) {
	records := make([]EventLogRecord, 0)
	for rows.Next() {
		record, err := scanEventLog(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event log rows: %w", err)
	}

	return records, nil
}

func scanEventLog(row interface{ Scan(...any) error }) (EventLogRecord, error) {
	var (
		record           EventLogRecord
		projectID        sql.NullString
		loopID           sql.NullString
		runID            sql.NullString
		entityType       sql.NullString
		entityID         sql.NullString
		correlationID    sql.NullString
		causationID      sql.NullString
		actorType        sql.NullString
		actorID          sql.NullString
		actorDisplayName sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&record.EventType,
		&projectID,
		&loopID,
		&runID,
		&entityType,
		&entityID,
		&correlationID,
		&causationID,
		&actorType,
		&actorID,
		&actorDisplayName,
		&record.PayloadJSON,
		&record.CreatedAt,
	)
	if err != nil {
		return EventLogRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.EntityType = nullableString(entityType)
	record.EntityID = nullableString(entityID)
	record.CorrelationID = nullableString(correlationID)
	record.CausationID = nullableString(causationID)
	record.ActorType = nullableString(actorType)
	record.ActorID = nullableString(actorID)
	record.ActorDisplayName = nullableString(actorDisplayName)

	return record, nil
}

func scanNotifications(rows *sql.Rows) ([]NotificationRecord, error) {
	records := make([]NotificationRecord, 0)
	for rows.Next() {
		record, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate notification rows: %w", err)
	}

	return records, nil
}

func scanNotification(row interface{ Scan(...any) error }) (NotificationRecord, error) {
	var (
		record       NotificationRecord
		projectID    sql.NullString
		loopID       sql.NullString
		runID        sql.NullString
		entityType   sql.NullString
		entityID     sql.NullString
		subtitle     sql.NullString
		dedupeKey    sql.NullString
		errorMessage sql.NullString
		payloadJSON  sql.NullString
		sentAt       sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&runID,
		&entityType,
		&entityID,
		&record.Channel,
		&record.Level,
		&record.Title,
		&subtitle,
		&record.Body,
		&record.Status,
		&dedupeKey,
		&errorMessage,
		&payloadJSON,
		&sentAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return NotificationRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.RunID = nullableString(runID)
	record.EntityType = nullableString(entityType)
	record.EntityID = nullableString(entityID)
	record.Subtitle = nullableString(subtitle)
	record.DedupeKey = nullableString(dedupeKey)
	record.ErrorMessage = nullableString(errorMessage)
	record.PayloadJSON = nullableString(payloadJSON)
	record.SentAt = nullableString(sentAt)

	return record, nil
}

func scanLocks(rows *sql.Rows) ([]LockRecord, error) {
	records := make([]LockRecord, 0)
	for rows.Next() {
		record, err := scanLock(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate lock rows: %w", err)
	}

	return records, nil
}

func scanLock(row interface{ Scan(...any) error }) (LockRecord, error) {
	var (
		record LockRecord
		reason sql.NullString
	)

	err := row.Scan(&record.Key, &record.Owner, &reason, &record.ExpiresAt, &record.CreatedAt, &record.UpdatedAt)
	if err != nil {
		return LockRecord{}, err
	}
	record.Reason = nullableString(reason)

	return record, nil
}

func scanWorktrees(rows *sql.Rows) ([]WorktreeRecord, error) {
	records := make([]WorktreeRecord, 0)
	for rows.Next() {
		record, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate worktree rows: %w", err)
	}

	return records, nil
}

func scanQueueItems(rows *sql.Rows) ([]QueueItemRecord, error) {
	records := make([]QueueItemRecord, 0)
	for rows.Next() {
		record, err := scanQueueItem(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate queue item rows: %w", err)
	}

	return records, nil
}

func scanSweeperCases(rows *sql.Rows) ([]SweeperCaseRecord, error) {
	records := make([]SweeperCaseRecord, 0)
	for rows.Next() {
		record, err := scanSweeperCase(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sweeper case rows: %w", err)
	}

	return records, nil
}

func scanSweeperCase(row interface{ Scan(...any) error }) (SweeperCaseRecord, error) {
	var (
		record                 SweeperCaseRecord
		currentCategory        sql.NullString
		currentConfidenceScore sql.NullInt64
		warningCommentID       sql.NullInt64
		warningMarkerUUID      sql.NullString
		lastProposalID         sql.NullString
		lastFingerprintJSON    sql.NullString
		lastHumanActivityAt    sql.NullString
		warnedAt               sql.NullString
		closeDueAt             sql.NullString
		terminalOutcome        sql.NullString
		terminalAt             sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&record.ProjectID,
		&record.Repo,
		&record.TargetType,
		&record.TargetNumber,
		&record.Status,
		&record.CurrentPhase,
		&currentCategory,
		&currentConfidenceScore,
		&warningCommentID,
		&warningMarkerUUID,
		&lastProposalID,
		&lastFingerprintJSON,
		&lastHumanActivityAt,
		&warnedAt,
		&closeDueAt,
		&terminalOutcome,
		&terminalAt,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return SweeperCaseRecord{}, err
	}

	record.CurrentCategory = nullableString(currentCategory)
	record.CurrentConfidenceScore = nullableInt64(currentConfidenceScore)
	record.WarningCommentID = nullableInt64(warningCommentID)
	record.WarningMarkerUUID = nullableString(warningMarkerUUID)
	record.LastProposalID = nullableString(lastProposalID)
	record.LastFingerprintJSON = nullableString(lastFingerprintJSON)
	record.LastHumanActivityAt = nullableString(lastHumanActivityAt)
	record.WarnedAt = nullableString(warnedAt)
	record.CloseDueAt = nullableString(closeDueAt)
	record.TerminalOutcome = nullableString(terminalOutcome)
	record.TerminalAt = nullableString(terminalAt)

	return record, nil
}

func scanSweeperProposals(rows *sql.Rows) ([]SweeperProposalRecord, error) {
	records := make([]SweeperProposalRecord, 0)
	for rows.Next() {
		record, err := scanSweeperProposal(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sweeper proposal rows: %w", err)
	}

	return records, nil
}

func scanSweeperProposal(row interface{ Scan(...any) error }) (SweeperProposalRecord, error) {
	var (
		record           SweeperProposalRecord
		rawResultJSON    sql.NullString
		summary          sql.NullString
		rationale        sql.NullString
		markerUUID       sql.NullString
		validationStatus sql.NullString
		validationError  sql.NullString
		applyStatus      sql.NullString
		applySummary     sql.NullString
		applyError       sql.NullString
		appliedAt        sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&record.CaseID,
		&record.ProjectID,
		&record.Repo,
		&record.TargetType,
		&record.TargetNumber,
		&record.SchemaVersion,
		&record.ProposerKind,
		&record.FactBundleJSON,
		&record.FingerprintJSON,
		&record.ProposalJSON,
		&rawResultJSON,
		&record.Decision,
		&record.Category,
		&record.ConfidenceScore,
		&summary,
		&rationale,
		&markerUUID,
		&validationStatus,
		&validationError,
		&applyStatus,
		&applySummary,
		&applyError,
		&appliedAt,
		&record.CreatedAt,
	)
	if err != nil {
		return SweeperProposalRecord{}, err
	}

	record.Summary = nullableString(summary)
	record.Rationale = nullableString(rationale)
	record.RawResultJSON = nullableString(rawResultJSON)
	record.MarkerUUID = nullableString(markerUUID)
	record.ValidationStatus = nullableString(validationStatus)
	record.ValidationError = nullableString(validationError)
	record.ApplyStatus = nullableString(applyStatus)
	record.ApplySummary = nullableString(applySummary)
	record.ApplyError = nullableString(applyError)
	record.AppliedAt = nullableString(appliedAt)

	return record, nil
}

func scanQueueItem(row interface{ Scan(...any) error }) (QueueItemRecord, error) {
	var (
		record        QueueItemRecord
		projectID     sql.NullString
		loopID        sql.NullString
		repo          sql.NullString
		prNumber      sql.NullInt64
		claimedBy     sql.NullString
		claimedAt     sql.NullString
		startedAt     sql.NullString
		finishedAt    sql.NullString
		lockKey       sql.NullString
		payloadJSON   sql.NullString
		lastError     sql.NullString
		lastErrorKind sql.NullString
	)

	err := row.Scan(
		&record.ID,
		&projectID,
		&loopID,
		&record.Type,
		&record.TargetType,
		&record.TargetID,
		&repo,
		&prNumber,
		&record.DedupeKey,
		&record.Priority,
		&record.Status,
		&record.AvailableAt,
		&record.Attempts,
		&record.MaxAttempts,
		&claimedBy,
		&claimedAt,
		&startedAt,
		&finishedAt,
		&lockKey,
		&payloadJSON,
		&lastError,
		&lastErrorKind,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return QueueItemRecord{}, err
	}

	record.ProjectID = nullableString(projectID)
	record.LoopID = nullableString(loopID)
	record.Repo = nullableString(repo)
	record.PRNumber = nullableInt64(prNumber)
	record.ClaimedBy = nullableString(claimedBy)
	record.ClaimedAt = nullableString(claimedAt)
	record.StartedAt = nullableString(startedAt)
	record.FinishedAt = nullableString(finishedAt)
	record.LockKey = nullableString(lockKey)
	record.PayloadJSON = nullableString(payloadJSON)
	record.LastError = nullableString(lastError)
	record.LastErrorKind = nullableString(lastErrorKind)

	return record, nil
}

func scanWorktree(row interface{ Scan(...any) error }) (WorktreeRecord, error) {
	var (
		record       WorktreeRecord
		baseBranch   sql.NullString
		headSHA      sql.NullString
		metadataJSON sql.NullString
		cleanedAt    sql.NullString
	)

	err := row.Scan(&record.ID, &record.ProjectID, &record.RepoPath, &record.WorktreePath, &record.Branch, &baseBranch, &record.Status, &headSHA, &metadataJSON, &record.CreatedAt, &record.UpdatedAt, &cleanedAt)
	if err != nil {
		return WorktreeRecord{}, err
	}
	record.BaseBranch = nullableString(baseBranch)
	record.HeadSHA = nullableString(headSHA)
	record.MetadataJSON = nullableString(metadataJSON)
	record.CleanedAt = nullableString(cleanedAt)

	return record, nil
}
