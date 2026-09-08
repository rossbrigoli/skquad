-- Owner-facing inbox notifications: task completions and action requests.
-- Separate from the agent message queue so owners only see what is addressed
-- to them.

CREATE TABLE IF NOT EXISTS inbox_messages (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    squad_id      uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    from_agent_id uuid REFERENCES agents(id) ON DELETE SET NULL,
    task_id       uuid REFERENCES tasks(id) ON DELETE SET NULL,
    kind          text NOT NULL CHECK (kind IN ('task_completed','action_required')),
    message       text NOT NULL DEFAULT '',
    read_at       timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_inbox_user_created ON inbox_messages(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_inbox_user_unread ON inbox_messages(user_id, created_at)
    WHERE read_at IS NULL;
