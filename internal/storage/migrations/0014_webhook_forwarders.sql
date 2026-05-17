CREATE TABLE IF NOT EXISTS webhook_forwarders (
  repo TEXT PRIMARY KEY,
  pid INTEGER NOT NULL,
  process_start INTEGER NOT NULL,
  fingerprint TEXT NOT NULL,
  endpoint TEXT NOT NULL,
  events TEXT NOT NULL,
  gh_path TEXT NOT NULL,
  daemon_id TEXT NOT NULL,
  spawned_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
