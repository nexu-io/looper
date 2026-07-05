-- Maps a Feishu message thread root to the loop whose notifications thread under
-- it. The forward direction (loop -> root) lets the gateway thread a task's
-- messages together; the reverse (root -> loop) lets the inbound callback route a
-- free-text reply typed in the thread back to the loop that asked.
CREATE TABLE IF NOT EXISTS feishu_threads (
  root_message_id TEXT PRIMARY KEY,
  loop_id TEXT NOT NULL,
  chat_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_feishu_threads_loop ON feishu_threads(loop_id);
