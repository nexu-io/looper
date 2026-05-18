CREATE TABLE IF NOT EXISTS webhook_tunnel_hooks (
  repo TEXT PRIMARY KEY,
  hook_id INTEGER NOT NULL,
  managed_url TEXT NOT NULL,
  secret_ref TEXT NOT NULL,
  last_ping_at INTEGER,
  consecutive_disables INTEGER NOT NULL DEFAULT 0,
  last_disable_at INTEGER,
  orphaned INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
