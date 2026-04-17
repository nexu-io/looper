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
	PullRequestSnapshots *PullRequestSnapshotsRepository
	Locks                *LocksRepository
	Worktrees            *WorktreesRepository
}

func NewRepositories(q sqliteQuerier) *Repositories {
	return &Repositories{
		Projects:             &ProjectsRepository{q: q},
		Loops:                &LoopsRepository{q: q},
		Runs:                 &RunsRepository{q: q},
		PullRequestSnapshots: &PullRequestSnapshotsRepository{q: q},
		Locks:                &LocksRepository{q: q, now: time.Now},
		Worktrees:            &WorktreesRepository{q: q},
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

type LockRecord struct {
	Key       string
	Owner     string
	Reason    *string
	ExpiresAt string
	CreatedAt string
	UpdatedAt string
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
	row := r.q.QueryRowContext(ctx, `SELECT * FROM runs WHERE loop_id = ? ORDER BY started_at DESC LIMIT 1`, loopID)
	record, err := scanRun(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get latest run by loop id: %w", err)
	}

	return &record, nil
}

func (r *RunsRepository) List(ctx context.Context) ([]RunRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
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

func (r *LocksRepository) ListExpired(ctx context.Context, nowISO string) ([]LockRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT * FROM locks WHERE expires_at <= ? ORDER BY expires_at ASC`, nowISO)
	if err != nil {
		return nil, fmt.Errorf("list expired locks: %w", err)
	}
	defer rows.Close()

	return scanLocks(rows)
}

type WorktreesRepository struct{ q sqliteQuerier }

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
