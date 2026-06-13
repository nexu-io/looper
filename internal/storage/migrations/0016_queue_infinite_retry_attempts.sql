PRAGMA foreign_keys = OFF;

CREATE TABLE queue_items_v3 (
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
  max_attempts INTEGER NOT NULL DEFAULT -1,
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
  CHECK (max_attempts = -1 OR max_attempts > 0),
  CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled', 'manual_intervention')),
  CHECK (last_error_kind IS NULL OR last_error_kind IN ('retryable_transient', 'retryable_after_resume', 'non_retryable', 'manual_intervention'))
);

INSERT INTO queue_items_v3 (
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
FROM queue_items;

DROP TABLE queue_items;
ALTER TABLE queue_items_v3 RENAME TO queue_items;

CREATE INDEX idx_queue_items_status_available_priority
  ON queue_items (status, available_at, priority, created_at);
CREATE INDEX idx_queue_items_loop_status
  ON queue_items (loop_id, status, updated_at DESC);
CREATE INDEX idx_queue_items_type_repo_pr_status
  ON queue_items (type, repo, pr_number, status, available_at);
CREATE INDEX idx_queue_items_dedupe_status
  ON queue_items (dedupe_key, status, updated_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_queue_items_one_active_dedupe
  ON queue_items (dedupe_key)
  WHERE type IN ('reviewer', 'fixer')
    AND status IN ('queued', 'running');

PRAGMA foreign_keys = ON;
