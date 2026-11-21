-- Persists each loop's live-progress feed message id (the tool-call feed reply
-- threaded under the anchor). It was in-memory only, so a daemon restart lost the
-- id and the next progress tick posted a brand-new feed card instead of patching
-- the existing one in place. Keyed per loop (the anchor is per task, but the feed
-- is per loop's execution), so it lives in its own table rather than a column on
-- feishu_threads.
CREATE TABLE IF NOT EXISTS feishu_live_feeds (
  loop_id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);
