PRAGMA foreign_keys = OFF;

CREATE TABLE loops_v2 (
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

INSERT INTO loops_v2 (
  id,
  project_id,
  type,
  target_type,
  target_id,
  repo,
  pr_number,
  status,
  config_json,
  metadata_json,
  last_run_at,
  next_run_at,
  created_at,
  updated_at
)
SELECT
  id,
  project_id,
  type,
  CASE
    WHEN type = 'worker' THEN 'project'
    WHEN target_type = 'pull_request' THEN 'pull_request'
    ELSE 'project'
  END AS target_type,
  CASE
    WHEN type = 'worker' THEN project_id
    WHEN target_type = 'pull_request' THEN target_id
    ELSE COALESCE(target_id, project_id)
  END AS target_id,
  repo,
  pr_number,
  CASE
    WHEN type = 'worker' AND status IN ('queued', 'running', 'idle') THEN 'paused'
    ELSE status
  END AS status,
  config_json,
  metadata_json,
  last_run_at,
  CASE
    WHEN type = 'worker' THEN NULL
    ELSE next_run_at
  END AS next_run_at,
  created_at,
  updated_at
FROM loops;

DROP TABLE loops;
ALTER TABLE loops_v2 RENAME TO loops;

CREATE INDEX idx_loops_status ON loops (status);
CREATE INDEX idx_loops_target ON loops (target_type, target_id);
CREATE INDEX idx_loops_repo_pr ON loops (repo, pr_number);
CREATE INDEX idx_loops_next_run_at ON loops (next_run_at);

CREATE TABLE queue_items_v2 (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  loop_id TEXT,
  type TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_id TEXT NOT NULL,
  repo TEXT,
  pr_number INTEGER,
  dedupe_key TEXT NOT NULL,
  priority INTEGER NOT NULL,
  status TEXT NOT NULL,
  available_at TEXT NOT NULL,
  attempts INTEGER NOT NULL DEFAULT 0,
  max_attempts INTEGER NOT NULL DEFAULT 3,
  claimed_by TEXT,
  claimed_at TEXT,
  started_at TEXT,
  finished_at TEXT,
  lock_key TEXT,
  payload_json TEXT,
  last_error TEXT,
  last_error_kind TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  FOREIGN KEY (loop_id) REFERENCES loops (id) ON DELETE CASCADE,
  CHECK (pr_number IS NULL OR pr_number > 0),
  CHECK (priority > 0),
  CHECK (attempts >= 0),
  CHECK (max_attempts >= 1),
  CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled', 'manual_intervention')),
  CHECK (last_error_kind IS NULL OR last_error_kind IN ('retryable_transient', 'retryable_after_resume', 'non_retryable', 'manual_intervention'))
);

INSERT INTO queue_items_v2 (
  id,
  project_id,
  loop_id,
  type,
  target_type,
  target_id,
  repo,
  pr_number,
  dedupe_key,
  priority,
  status,
  available_at,
  attempts,
  max_attempts,
  claimed_by,
  claimed_at,
  started_at,
  finished_at,
  lock_key,
  payload_json,
  last_error,
  last_error_kind,
  created_at,
  updated_at
)
SELECT
  id,
  project_id,
  loop_id,
  type,
  CASE
    WHEN type = 'worker' THEN 'project'
    ELSE target_type
  END AS target_type,
  CASE
    WHEN type = 'worker' THEN COALESCE(project_id, target_id)
    ELSE target_id
  END AS target_id,
  repo,
  pr_number,
  CASE
    WHEN type = 'worker' AND loop_id IS NOT NULL THEN 'worker:' || loop_id
    ELSE dedupe_key
  END AS dedupe_key,
  priority,
  CASE
    WHEN type = 'worker' AND status IN ('queued', 'running') THEN 'cancelled'
    ELSE status
  END AS status,
  available_at,
  attempts,
  max_attempts,
  claimed_by,
  claimed_at,
  started_at,
  CASE
    WHEN type = 'worker' AND status IN ('queued', 'running') THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    ELSE finished_at
  END AS finished_at,
  CASE
    WHEN type = 'worker' AND loop_id IS NOT NULL THEN 'worker:' || loop_id
    ELSE lock_key
  END AS lock_key,
  payload_json,
  CASE
    WHEN type = 'worker' AND status IN ('queued', 'running') THEN COALESCE(last_error, 'Cancelled during worker project-target migration')
    ELSE last_error
  END AS last_error,
  last_error_kind,
  created_at,
  updated_at
FROM queue_items;

DROP TABLE queue_items;
ALTER TABLE queue_items_v2 RENAME TO queue_items;

CREATE INDEX idx_queue_items_status_available_priority
  ON queue_items (status, available_at, priority, created_at);
CREATE INDEX idx_queue_items_loop_status
  ON queue_items (loop_id, status, updated_at DESC);
CREATE INDEX idx_queue_items_type_repo_pr_status
  ON queue_items (type, repo, pr_number, status, available_at);
CREATE INDEX idx_queue_items_dedupe_status
  ON queue_items (dedupe_key, status, updated_at DESC);

CREATE TABLE agent_executions_v2 (
  id TEXT PRIMARY KEY,
  project_id TEXT,
  loop_id TEXT,
  run_id TEXT,
  vendor TEXT NOT NULL,
  status TEXT NOT NULL,
  pid INTEGER,
  command_json TEXT,
  cwd TEXT,
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

INSERT INTO agent_executions_v2 (
  id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd,
  summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at,
  output_json, error_message, started_at, ended_at, metadata_json, created_at,
  updated_at
)
SELECT
  id, project_id, loop_id, run_id, vendor, status, pid, command_json, cwd,
  summary, parse_status, completion_signal, heartbeat_count, last_heartbeat_at,
  output_json, error_message, started_at, ended_at, metadata_json, created_at,
  updated_at
FROM agent_executions;

DROP TABLE agent_executions;
ALTER TABLE agent_executions_v2 RENAME TO agent_executions;

CREATE INDEX idx_agent_executions_status_started_at
  ON agent_executions (status, started_at DESC);
CREATE INDEX idx_agent_executions_loop_started_at
  ON agent_executions (loop_id, started_at DESC);

CREATE TABLE worktrees_v2 (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  repo_path TEXT NOT NULL,
  worktree_path TEXT NOT NULL,
  branch TEXT NOT NULL,
  base_branch TEXT,
  status TEXT NOT NULL,
  head_sha TEXT,
  metadata_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  cleaned_at TEXT,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE
);

INSERT INTO worktrees_v2 (
  id, project_id, repo_path, worktree_path, branch, base_branch, status,
  head_sha, metadata_json, created_at, updated_at, cleaned_at
)
SELECT
  id, project_id, repo_path, worktree_path, branch, base_branch, status,
  head_sha, metadata_json, created_at, updated_at, cleaned_at
FROM worktrees;

DROP TABLE worktrees;
ALTER TABLE worktrees_v2 RENAME TO worktrees;

CREATE INDEX idx_worktrees_project_status
  ON worktrees (project_id, status, updated_at DESC);
CREATE UNIQUE INDEX idx_worktrees_project_branch
  ON worktrees (project_id, branch);

DROP TABLE IF EXISTS task_items;
DROP TABLE IF EXISTS tasks;

PRAGMA foreign_keys = ON;
