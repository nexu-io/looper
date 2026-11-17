-- Device ownership and revocation stay in Plane. CF stores only receipt time.
CREATE TABLE IF NOT EXISTS devices (
  device_id TEXT PRIMARY KEY,
  node_id TEXT NOT NULL,
  last_heartbeat_at TEXT NOT NULL
);
