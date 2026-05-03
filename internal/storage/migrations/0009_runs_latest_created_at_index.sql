DROP INDEX IF EXISTS idx_runs_loop_id_started_at;

CREATE INDEX IF NOT EXISTS idx_runs_loop_id_started_at
  ON runs (loop_id, started_at DESC, created_at DESC);
