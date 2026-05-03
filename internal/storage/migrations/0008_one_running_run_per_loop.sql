UPDATE runs
SET status = 'interrupted',
    error_message = COALESCE(error_message, 'Interrupted by migration before enforcing one running run per loop'),
    ended_at = COALESCE(ended_at, updated_at),
    updated_at = COALESCE(updated_at, started_at)
WHERE status = 'running'
  AND EXISTS (
    SELECT 1
    FROM runs newer
    WHERE newer.loop_id = runs.loop_id
      AND (
        newer.started_at > runs.started_at
        OR (
          newer.started_at = runs.started_at
          AND (
            newer.created_at > runs.created_at
            OR (newer.created_at = runs.created_at AND newer.id > runs.id)
          )
        )
      )
  );

UPDATE runs
SET status = 'interrupted',
    error_message = COALESCE(error_message, 'Interrupted by migration because parent loop is terminal'),
    ended_at = COALESCE(ended_at, updated_at),
    updated_at = COALESCE(updated_at, started_at)
WHERE status = 'running'
  AND EXISTS (
    SELECT 1
    FROM loops
    WHERE loops.id = runs.loop_id
      AND loops.status IN ('stopped', 'terminated', 'completed', 'failed', 'interrupted')
  );

CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_one_running_per_loop
  ON runs (loop_id)
  WHERE status = 'running';
