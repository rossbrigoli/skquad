-- 0025: task Result field + richer inbox messages (S-181).
--
-- tasks.result persists the final outcome text of the latest terminal
-- transition: the completion summary for done tasks, the blocked reason
-- for blocked tasks. result_status records which status produced the
-- text (so the UI can label it correctly after manual moves) and
-- result_at when it was recorded.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS result text NOT NULL DEFAULT '';

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS result_status text NOT NULL DEFAULT '';

ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS result_at timestamptz;

-- inbox_messages gains an email-style subject and body. `message` stays
-- the one-line summary for backward compatibility; subject/body are
-- populated for task completed/blocked notifications and empty for
-- older or plain messages.
ALTER TABLE inbox_messages
    ADD COLUMN IF NOT EXISTS subject text NOT NULL DEFAULT '';

ALTER TABLE inbox_messages
    ADD COLUMN IF NOT EXISTS body text NOT NULL DEFAULT '';
