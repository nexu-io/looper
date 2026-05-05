ALTER TABLE agent_executions ADD COLUMN native_session_id TEXT;
ALTER TABLE agent_executions ADD COLUMN native_resume_mode TEXT;
ALTER TABLE agent_executions ADD COLUMN native_resume_status TEXT;
ALTER TABLE agent_executions ADD COLUMN native_resume_error TEXT;

CREATE INDEX idx_agent_executions_loop_native_resume
  ON agent_executions (loop_id, native_session_id, started_at DESC);
