-- 0029: S-193 epic — inbox & notifications.
--
-- (1) inbox_messages.kind gains 'agent_message': content a human explicitly
-- asked an agent to deliver to their inbox via the send_inbox tool. The
-- existing kinds stay system-emitted only.
ALTER TABLE inbox_messages
    DROP CONSTRAINT IF EXISTS inbox_messages_kind_check;
ALTER TABLE inbox_messages
    ADD CONSTRAINT inbox_messages_kind_check
    CHECK (kind IN ('task_completed','action_required','agent_message'));

-- (2) notifications: transient "something is going wrong" alerts for the
-- recipient (task failed / stuck, agent died, task blocked needing input).
-- Separate from the inbox by design: an inbox message never creates a
-- notification row and vice-versa. Scoped to the recipient user; the
-- admin "see all inboxes/notifications" filter is a handler-level query
-- param gated by the platform_admin role.
CREATE TABLE IF NOT EXISTS notifications (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    squad_id   uuid NOT NULL REFERENCES squads(id) ON DELETE CASCADE,
    task_id    uuid REFERENCES tasks(id) ON DELETE SET NULL,
    agent_id   uuid REFERENCES agents(id) ON DELETE SET NULL,
    type       text NOT NULL CHECK (type IN ('task_failed','task_stuck','agent_died','task_blocked')),
    severity   text NOT NULL DEFAULT 'warning' CHECK (severity IN ('info','warning','error')),
    message    text NOT NULL,
    read_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_notifications_user_created ON notifications(user_id, created_at);
CREATE INDEX IF NOT EXISTS idx_notifications_user_unread ON notifications(user_id, created_at)
    WHERE read_at IS NULL;

-- (3) send_inbox builtin (same pattern as send_message in 0023): the
-- blast radius is bounded — an agent can only write into its own squad
-- owner's inbox, capped length, no auto-action triggered by the message.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox'));

INSERT INTO builtin_tools_config (name, enabled) VALUES ('send_inbox', TRUE)
    ON CONFLICT (name) DO NOTHING;
