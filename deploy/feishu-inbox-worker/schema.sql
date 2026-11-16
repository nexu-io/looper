-- D1 schema for the Feishu → looper event inbox.
CREATE TABLE IF NOT EXISTS events (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  received_at    TEXT NOT NULL,
  kind           TEXT NOT NULL,              -- "message" | "card_action"
  chat_id        TEXT NOT NULL DEFAULT '',
  root_id        TEXT NOT NULL DEFAULT '',   -- thread root message id (maps to feishu_threads)
  thread_id      TEXT NOT NULL DEFAULT '',
  sender_open_id TEXT NOT NULL DEFAULT '',
  text           TEXT NOT NULL DEFAULT '',
  value_json     TEXT NOT NULL DEFAULT ''    -- card action {loopSeq,answer}
);
CREATE INDEX IF NOT EXISTS idx_events_root ON events(root_id);
