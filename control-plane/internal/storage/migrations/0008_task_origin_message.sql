-- 0008: origin linkage for tasks materialized from delegate/handoff messages.
-- A delegated task records the message that triggered it so completion can
-- notify the requesting agent and squad owner.
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS origin_message_id text NOT NULL DEFAULT '';
