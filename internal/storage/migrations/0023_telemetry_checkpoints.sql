CREATE TABLE telemetry_identity (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  database_id TEXT NOT NULL
);
CREATE TABLE telemetry_checkpoints (
  destination TEXT NOT NULL,
  loop_id TEXT NOT NULL REFERENCES loops(id) ON DELETE CASCADE,
  revision INTEGER NOT NULL,
  fingerprint TEXT NOT NULL,
  payload TEXT NOT NULL,
  acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0, 1)),
  PRIMARY KEY (destination, loop_id)
);
