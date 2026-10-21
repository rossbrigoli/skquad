-- 0058: S-282 session inspection builtins (sessions_list / sessions_history /
-- session_status).
--
-- Read-only inspection tools mirroring OpenClaw's sessions_* surface. They
-- add NO new server capabilities: every read rides an existing agent-
-- scoped endpoint the calling agent's bearer credential already authorizes
-- (GET /api/v1/agents/me/tasks, /agents/me/peers,
-- /agents/me/messages/history, /agents/me/tasks/{id}/context,
-- /agents/{agentID}/chat, /agents/me/model). The server's existing
-- authorization IS the visibility boundary — the tools never widen it,
-- surface 403/404 as clean errors, and sanitize rendered output
-- (secret-shaped values redacted, long bodies truncated) runtime-side.
--
-- Widen the builtin_tools_config name CHECK to admit the three tools
-- (same pattern as 0055/0056/0057) and seed them ENABLED: read-only,
-- self-scoped, invisible-if-disabled would defeat the purpose (mirrors
-- domain.BuiltinToolDefaultEnabled).
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process','cron','sessions_list','sessions_history','session_status'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('sessions_list', TRUE, '{}'),
        ('sessions_history', TRUE, '{}'),
        ('session_status', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
