-- 0030: notify_owner builtin tool (platform-prompt awareness extension).
--
-- Widens the builtin_tools_config name CHECK to admit 'notify_owner' and
-- seeds it ENABLED. The endpoint it fronts (POST /api/v1/agents/me/notify-owner)
-- already exists: it files an action_required InboxMessage to the agent's
-- squad owner, audited, 404 when the squad has no owner. Blast radius is
-- one capped-length inbox row in the agent's OWN squad owner's inbox —
-- no auto-action, no cross-squad reach (same posture as send_inbox in 0029).
-- Policy keys mirror send_message: timeoutSeconds, maxMessageChars
-- (maxMessageChars mirrors the server-side maxInboxMessageChars = 2000).
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES ('notify_owner', TRUE, '{"timeoutSeconds": 15, "maxMessageChars": 2000}')
    ON CONFLICT (name) DO NOTHING;
