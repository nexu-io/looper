-- Retain observations and completed-task review requests across network failures.
CREATE TABLE memory_operations (
 id TEXT PRIMARY KEY,
 project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
 loop_id TEXT REFERENCES loops(id) ON DELETE CASCADE,
 payload TEXT NOT NULL,
 acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1)),
 created_at TEXT NOT NULL,
 acknowledged_at TEXT,
 error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX memory_operations_pending ON memory_operations(acknowledged, created_at);
