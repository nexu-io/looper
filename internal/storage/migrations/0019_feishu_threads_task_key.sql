-- Adds a stable task identity to the Feishu thread anchor so the planner, worker,
-- reviewer and fixer loops spawned for ONE work item collapse onto a single card
-- instead of each posting its own. task_key is the originating issue (issue:repo:N),
-- derived from loop metadata at anchor time; it is NULL for loops with no source
-- issue (PR-triggered, project-level, issue-less bug), which keep per-loop keying.
ALTER TABLE feishu_threads ADD COLUMN task_key TEXT;

CREATE INDEX IF NOT EXISTS idx_feishu_threads_task ON feishu_threads(task_key);
