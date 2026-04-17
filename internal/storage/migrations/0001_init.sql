CREATE TABLE IF NOT EXISTS schema_migrations (
  id TEXT PRIMARY KEY,
  applied_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  repo_path TEXT NOT NULL,
  base_branch TEXT,
  archived INTEGER NOT NULL DEFAULT 0,
  metadata_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (archived IN (0, 1))
);

CREATE INDEX IF NOT EXISTS idx_projects_archived ON projects (archived);

CREATE TABLE IF NOT EXISTS loops (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  type TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT,
  repo TEXT,
  pr_number INTEGER,
  status TEXT NOT NULL,
  config_json TEXT,
  metadata_json TEXT,
  last_run_at TEXT,
  next_run_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  CHECK (target_type IN ('project', 'pull_request')),
  CHECK (pr_number IS NULL OR pr_number > 0)
);

CREATE INDEX IF NOT EXISTS idx_loops_status ON loops (status);
CREATE INDEX IF NOT EXISTS idx_loops_target ON loops (target_type, target_id);
CREATE INDEX IF NOT EXISTS idx_loops_repo_pr ON loops (repo, pr_number);
CREATE INDEX IF NOT EXISTS idx_loops_next_run_at ON loops (next_run_at);

CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  loop_id TEXT NOT NULL,
  status TEXT NOT NULL,
  current_step TEXT,
  last_completed_step TEXT,
  checkpoint_json TEXT,
  summary TEXT,
  error_message TEXT,
  started_at TEXT NOT NULL,
  last_heartbeat_at TEXT,
  ended_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (loop_id) REFERENCES loops (id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_runs_loop_id_started_at ON runs (loop_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs (status);

CREATE TABLE IF NOT EXISTS locks (
  key TEXT PRIMARY KEY,
  owner TEXT NOT NULL,
  reason TEXT,
  expires_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_locks_expires_at ON locks (expires_at);

CREATE TABLE IF NOT EXISTS event_logs (
  id TEXT PRIMARY KEY,
  event_type TEXT NOT NULL,
  project_id TEXT,
  loop_id TEXT,
  run_id TEXT,
  entity_type TEXT,
  entity_id TEXT,
  correlation_id TEXT,
  causation_id TEXT,
  actor_type TEXT,
  actor_id TEXT,
  actor_display_name TEXT,
  payload_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE SET NULL,
  FOREIGN KEY (loop_id) REFERENCES loops (id) ON DELETE SET NULL,
  FOREIGN KEY (run_id) REFERENCES runs (id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS idx_event_logs_entity_created_at ON event_logs (entity_type, entity_id, created_at);
CREATE INDEX IF NOT EXISTS idx_event_logs_type_created_at ON event_logs (event_type, created_at);

CREATE TABLE IF NOT EXISTS pull_request_snapshots (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  repo TEXT NOT NULL,
  pr_number INTEGER NOT NULL,
  head_sha TEXT NOT NULL,
  base_sha TEXT,
  title TEXT,
  body TEXT,
  author TEXT,
  diff_ref TEXT,
  checks_summary TEXT,
  unresolved_thread_count INTEGER,
  review_state TEXT,
  payload_json TEXT,
  captured_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  CHECK (pr_number > 0)
);

CREATE INDEX IF NOT EXISTS idx_pull_request_snapshots_repo_pr ON pull_request_snapshots (repo, pr_number, captured_at DESC);
