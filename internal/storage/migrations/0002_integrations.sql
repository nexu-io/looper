CREATE TABLE IF NOT EXISTS agent_executions (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  loop_id TEXT,
  run_id TEXT,
  vendor TEXT NOT NULL,
  status TEXT NOT NULL,
  pid INTEGER,
  command_json TEXT NOT NULL,
  cwd TEXT NOT NULL,
  summary TEXT,
  parse_status TEXT,
  completion_signal TEXT,
  heartbeat_count INTEGER NOT NULL DEFAULT 0,
  last_heartbeat_at TEXT,
  output_json TEXT,
  error_message TEXT,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  metadata_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE SET NULL,
  FOREIGN KEY (loop_id) REFERENCES loops (id) ON DELETE SET NULL,
  FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_agent_executions_status ON agent_executions (status);
CREATE INDEX IF NOT EXISTS idx_agent_executions_run ON agent_executions (run_id, started_at DESC);

CREATE TABLE IF NOT EXISTS notifications (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  loop_id TEXT,
  run_id TEXT,
  entity_type TEXT,
  entity_id TEXT,
  channel TEXT NOT NULL,
  level TEXT NOT NULL,
  title TEXT NOT NULL,
  subtitle TEXT,
  body TEXT NOT NULL,
  status TEXT NOT NULL,
  dedupe_key TEXT,
  error_message TEXT,
  payload_json TEXT,
  sent_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE SET NULL,
  FOREIGN KEY (loop_id) REFERENCES loops (id) ON DELETE SET NULL,
  FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_notifications_entity_created_at ON notifications (entity_type, entity_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_notifications_dedupe ON notifications (channel, dedupe_key, created_at DESC);

CREATE TABLE IF NOT EXISTS worktrees (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  repo_path TEXT NOT NULL,
  worktree_path TEXT NOT NULL,
  branch TEXT NOT NULL,
  base_branch TEXT NOT NULL,
  status TEXT NOT NULL,
  head_sha TEXT,
  metadata_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cleaned_at TEXT,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_worktrees_project_branch ON worktrees (project_id, branch);
CREATE UNIQUE INDEX IF NOT EXISTS idx_worktrees_path ON worktrees (worktree_path);
