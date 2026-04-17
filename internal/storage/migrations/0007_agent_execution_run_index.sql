CREATE INDEX IF NOT EXISTS idx_agent_executions_run
  ON agent_executions (run_id, started_at DESC);
