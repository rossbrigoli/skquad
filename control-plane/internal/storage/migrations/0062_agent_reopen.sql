-- S-290 (ADR-0016 D3): agent-driven reopen support on task_executions.
--
-- reviewer_feedback carries the reviewer's message (verbatim) into the new
-- attempt created by POST /agents/me/tasks/{id}/reopen, so the task brief
-- for the reopened attempt can include it.
--
-- reopen_seq marks attempts created by an agent reopen. The rolling-24h
-- reopen cap counts only these rows, so the cap governs AGENT reopens
-- only — human board moves never touch task_executions and stay unlimited.
ALTER TABLE task_executions ADD COLUMN IF NOT EXISTS reviewer_feedback TEXT NOT NULL DEFAULT '';
ALTER TABLE task_executions ADD COLUMN IF NOT EXISTS reopen_seq BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_task_executions_task_reopen
    ON task_executions(task_id, reopen_seq, started_at);
