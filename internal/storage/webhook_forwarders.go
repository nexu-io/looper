package storage

import (
	"context"
	"database/sql"
)

type WebhookForwarderRecord struct {
	Repo         string
	PID          int64
	ProcessStart int64
	Fingerprint  string
	Endpoint     string
	Events       string
	GHPath       string
	DaemonID     string
	SpawnedAt    int64
	UpdatedAt    int64
}

type WebhookForwardersRepository struct {
	q sqliteQuerier
}

func (r *WebhookForwardersRepository) List(ctx context.Context) ([]WebhookForwarderRecord, error) {
	rows, err := r.q.QueryContext(ctx, `
		SELECT repo, pid, process_start, fingerprint, endpoint, events, gh_path, daemon_id, spawned_at, updated_at
		FROM webhook_forwarders
		ORDER BY repo
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	records := []WebhookForwarderRecord{}
	for rows.Next() {
		var record WebhookForwarderRecord
		if err := rows.Scan(&record.Repo, &record.PID, &record.ProcessStart, &record.Fingerprint, &record.Endpoint, &record.Events, &record.GHPath, &record.DaemonID, &record.SpawnedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (r *WebhookForwardersRepository) Upsert(ctx context.Context, record WebhookForwarderRecord) error {
	_, err := r.q.ExecContext(ctx, `
		INSERT INTO webhook_forwarders (repo, pid, process_start, fingerprint, endpoint, events, gh_path, daemon_id, spawned_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo) DO UPDATE SET
			pid = excluded.pid,
			process_start = excluded.process_start,
			fingerprint = excluded.fingerprint,
			endpoint = excluded.endpoint,
			events = excluded.events,
			gh_path = excluded.gh_path,
			daemon_id = excluded.daemon_id,
			spawned_at = excluded.spawned_at,
			updated_at = excluded.updated_at
	`, record.Repo, record.PID, record.ProcessStart, record.Fingerprint, record.Endpoint, record.Events, record.GHPath, record.DaemonID, record.SpawnedAt, record.UpdatedAt)
	return err
}

func (r *WebhookForwardersRepository) Delete(ctx context.Context, repo string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM webhook_forwarders WHERE repo = ?`, repo)
	if err == sql.ErrNoRows {
		return nil
	}
	return err
}
