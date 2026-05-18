package storage

import (
	"context"
	"database/sql"
	"errors"
)

type WebhookTunnelHookRecord struct {
	Repo                string
	HookID              int64
	ManagedURL          string
	SecretRef           string
	LastPingAt          *int64
	ConsecutiveDisables int64
	LastDisableAt       *int64
	Orphaned            bool
	CreatedAt           int64
	UpdatedAt           int64
}

type WebhookTunnelHooksRepository struct{ q sqliteQuerier }

func (r *WebhookTunnelHooksRepository) List(ctx context.Context) ([]WebhookTunnelHookRecord, error) {
	rows, err := r.q.QueryContext(ctx, `SELECT repo, hook_id, managed_url, secret_ref, last_ping_at, consecutive_disables, last_disable_at, orphaned, created_at, updated_at FROM webhook_tunnel_hooks ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []WebhookTunnelHookRecord
	for rows.Next() {
		var record WebhookTunnelHookRecord
		var orphaned int64
		if err := rows.Scan(&record.Repo, &record.HookID, &record.ManagedURL, &record.SecretRef, &record.LastPingAt, &record.ConsecutiveDisables, &record.LastDisableAt, &orphaned, &record.CreatedAt, &record.UpdatedAt); err != nil {
			return nil, err
		}
		record.Orphaned = orphaned != 0
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func (r *WebhookTunnelHooksRepository) Get(ctx context.Context, repo string) (WebhookTunnelHookRecord, bool, error) {
	var record WebhookTunnelHookRecord
	var orphaned int64
	err := r.q.QueryRowContext(ctx, `SELECT repo, hook_id, managed_url, secret_ref, last_ping_at, consecutive_disables, last_disable_at, orphaned, created_at, updated_at FROM webhook_tunnel_hooks WHERE repo = ?`, repo).Scan(&record.Repo, &record.HookID, &record.ManagedURL, &record.SecretRef, &record.LastPingAt, &record.ConsecutiveDisables, &record.LastDisableAt, &orphaned, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookTunnelHookRecord{}, false, nil
	}
	if err != nil {
		return WebhookTunnelHookRecord{}, false, err
	}
	record.Orphaned = orphaned != 0
	return record, true, nil
}

func (r *WebhookTunnelHooksRepository) Upsert(ctx context.Context, record WebhookTunnelHookRecord) error {
	orphaned := int64(0)
	if record.Orphaned {
		orphaned = 1
	}
	_, err := r.q.ExecContext(ctx, `INSERT INTO webhook_tunnel_hooks (repo, hook_id, managed_url, secret_ref, last_ping_at, consecutive_disables, last_disable_at, orphaned, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(repo) DO UPDATE SET hook_id = excluded.hook_id, managed_url = excluded.managed_url, secret_ref = excluded.secret_ref, last_ping_at = excluded.last_ping_at, consecutive_disables = excluded.consecutive_disables, last_disable_at = excluded.last_disable_at, orphaned = excluded.orphaned, created_at = excluded.created_at, updated_at = excluded.updated_at`, record.Repo, record.HookID, record.ManagedURL, record.SecretRef, record.LastPingAt, record.ConsecutiveDisables, record.LastDisableAt, orphaned, record.CreatedAt, record.UpdatedAt)
	return err
}

func (r *WebhookTunnelHooksRepository) MarkOrphaned(ctx context.Context, repo string, orphaned bool, updatedAt int64) error {
	orphanedInt := int64(0)
	if orphaned {
		orphanedInt = 1
	}
	_, err := r.q.ExecContext(ctx, `UPDATE webhook_tunnel_hooks SET orphaned = ?, updated_at = ? WHERE repo = ?`, orphanedInt, updatedAt, repo)
	return err
}

func (r *WebhookTunnelHooksRepository) Delete(ctx context.Context, repo string) error {
	_, err := r.q.ExecContext(ctx, `DELETE FROM webhook_tunnel_hooks WHERE repo = ?`, repo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func (r *WebhookTunnelHooksRepository) UpdatePing(ctx context.Context, repo string, at int64) error {
	_, err := r.q.ExecContext(ctx, `UPDATE webhook_tunnel_hooks SET last_ping_at = ?, updated_at = ? WHERE repo = ?`, at, at, repo)
	return err
}
