CREATE TABLE IF NOT EXISTS sweeper_cases (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  repo TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_number INTEGER NOT NULL,
  status TEXT NOT NULL,
  current_phase TEXT NOT NULL,
  current_category TEXT,
  current_confidence_score INTEGER,
  warning_comment_id INTEGER,
  warning_marker_uuid TEXT,
  last_proposal_id TEXT,
  last_fingerprint_json TEXT,
  last_human_activity_at TEXT,
  warned_at TEXT,
  close_due_at TEXT,
  terminal_outcome TEXT,
  terminal_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  CHECK (target_type IN ('issue', 'pull_request')),
  CHECK (target_number > 0),
  CHECK (status IN ('open', 'pending', 'terminal', 'cancelled', 'quarantined')),
  CHECK (current_phase IN ('prefilter', 'warn', 'close', 'reconcile', 'terminal')),
  CHECK (current_confidence_score IS NULL OR (current_confidence_score >= 0 AND current_confidence_score <= 100)),
  CHECK (warning_comment_id IS NULL OR warning_comment_id > 0)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_sweeper_cases_project_repo_target
  ON sweeper_cases (project_id, repo, target_type, target_number);
CREATE INDEX IF NOT EXISTS idx_sweeper_cases_project_repo_phase
  ON sweeper_cases (project_id, repo, current_phase);
CREATE INDEX IF NOT EXISTS idx_sweeper_cases_project_repo_status
  ON sweeper_cases (project_id, repo, status);
CREATE INDEX IF NOT EXISTS idx_sweeper_cases_target_lookup
  ON sweeper_cases (target_type, target_number, repo);

CREATE TABLE IF NOT EXISTS sweeper_proposals (
  id TEXT PRIMARY KEY,
  case_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  repo TEXT NOT NULL,
  target_type TEXT NOT NULL,
  target_number INTEGER NOT NULL,
  schema_version INTEGER NOT NULL,
  proposer_kind TEXT NOT NULL,
  fact_bundle_json TEXT NOT NULL,
  fingerprint_json TEXT NOT NULL,
  proposal_json TEXT NOT NULL,
  decision TEXT NOT NULL,
  category TEXT NOT NULL,
  confidence_score INTEGER NOT NULL,
  summary TEXT,
  rationale TEXT,
  marker_uuid TEXT,
  validation_status TEXT,
  validation_error TEXT,
  apply_status TEXT,
  apply_summary TEXT,
  apply_error TEXT,
  applied_at TEXT,
  created_at TEXT NOT NULL,
  FOREIGN KEY (case_id) REFERENCES sweeper_cases (id) ON DELETE CASCADE,
  FOREIGN KEY (project_id) REFERENCES projects (id) ON DELETE CASCADE,
  CHECK (target_type IN ('issue', 'pull_request')),
  CHECK (target_number > 0),
  CHECK (schema_version > 0),
  CHECK (confidence_score >= 0 AND confidence_score <= 100),
  CHECK (decision IN ('no_action', 'warn', 'close', 'cancel', 'quarantine', 'stale_proposal'))
);

CREATE INDEX IF NOT EXISTS idx_sweeper_proposals_case_created
  ON sweeper_proposals (case_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_sweeper_proposals_project_repo_applied
  ON sweeper_proposals (project_id, repo, applied_at);
CREATE INDEX IF NOT EXISTS idx_sweeper_proposals_apply_status
  ON sweeper_proposals (apply_status);
