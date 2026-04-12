PRAGMA foreign_keys = OFF;

CREATE TABLE loops_v3 (
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
  CHECK (target_type IN ('project', 'pull_request', 'issue')),
  CHECK (pr_number IS NULL OR pr_number > 0)
);

INSERT INTO loops_v3 (
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
FROM loops;

DROP TABLE loops;
ALTER TABLE loops_v3 RENAME TO loops;

CREATE INDEX idx_loops_status ON loops (status);
CREATE INDEX idx_loops_target ON loops (target_type, target_id);
CREATE INDEX idx_loops_repo_pr ON loops (repo, pr_number);
CREATE INDEX idx_loops_next_run_at ON loops (next_run_at);

PRAGMA foreign_keys = ON;
