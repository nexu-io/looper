UPDATE queue_items
SET status = 'cancelled',
    finished_at = COALESCE(finished_at, updated_at, created_at, available_at),
    last_error = COALESCE(last_error, 'Cancelled by migration before enforcing one active queue item per dedupe key'),
    last_error_kind = COALESCE(last_error_kind, 'non_retryable'),
    updated_at = COALESCE(updated_at, created_at, available_at)
WHERE type IN ('reviewer', 'fixer')
  AND status IN ('queued', 'running')
  AND EXISTS (
    SELECT 1
    FROM loops
    WHERE loops.id = queue_items.loop_id
      AND loops.status IN ('paused', 'completed', 'failed', 'interrupted', 'terminated', 'stopped')
  );

UPDATE queue_items
SET status = 'cancelled',
    finished_at = COALESCE(finished_at, updated_at, created_at, available_at),
    last_error = COALESCE(last_error, 'Cancelled by migration before enforcing one active queue item per dedupe key'),
    last_error_kind = COALESCE(last_error_kind, 'non_retryable'),
    updated_at = COALESCE(updated_at, created_at, available_at)
WHERE status IN ('queued', 'running')
  AND type IN ('reviewer', 'fixer')
  AND EXISTS (
    SELECT 1
    FROM queue_items preferred
    WHERE preferred.dedupe_key = queue_items.dedupe_key
      AND preferred.type IN ('reviewer', 'fixer')
      AND preferred.status IN ('queued', 'running')
      AND (
        CASE preferred.status WHEN 'running' THEN 1 ELSE 0 END > CASE queue_items.status WHEN 'running' THEN 1 ELSE 0 END
        OR (
          CASE preferred.status WHEN 'running' THEN 1 ELSE 0 END = CASE queue_items.status WHEN 'running' THEN 1 ELSE 0 END
          AND (
            preferred.updated_at > queue_items.updated_at
            OR (
              preferred.updated_at = queue_items.updated_at
              AND (
                preferred.created_at > queue_items.created_at
                OR (
                  preferred.created_at = queue_items.created_at
                  AND preferred.id > queue_items.id
                )
              )
            )
          )
        )
      )
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_items_one_active_dedupe
  ON queue_items (dedupe_key)
  WHERE type IN ('reviewer', 'fixer')
    AND status IN ('queued', 'running');
