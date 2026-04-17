PRAGMA foreign_keys = OFF;

CREATE TABLE loops_v4 (
  id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
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

INSERT INTO loops_v4 (
  id,
  seq,
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
  ROW_NUMBER() OVER (ORDER BY created_at ASC, id ASC) AS seq,
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
ALTER TABLE loops_v4 RENAME TO loops;

CREATE UNIQUE INDEX idx_loops_seq ON loops (seq);
CREATE INDEX idx_loops_status ON loops (status);
CREATE INDEX idx_loops_target ON loops (target_type, target_id);
CREATE INDEX idx_loops_repo_pr ON loops (repo, pr_number);
CREATE INDEX idx_loops_next_run_at ON loops (next_run_at);

CREATE TABLE IF NOT EXISTS counters (
  name TEXT PRIMARY KEY,
  value INTEGER NOT NULL
);

INSERT INTO counters (name, value)
VALUES ('loop_seq', COALESCE((SELECT MAX(seq) FROM loops), 0))
ON CONFLICT(name) DO UPDATE SET value = excluded.value;

PRAGMA foreign_keys = ON;
