-- 0055: S-279 file tools builtin (read / write / edit / apply_patch).
--
-- Widens the builtin_tools_config name CHECK to admit the four FILE tools
-- and seeds them ENABLED. They give agents first-class coding capability
-- in their task workspace (the same workspace_dir the exec tool runs in),
-- mirroring the OpenClaw read/write/edit/apply_patch contract. The
-- security boundary is runtime-side: every path is realpath-resolved and
-- rejected when it escapes the workspace (symlinks and '..' included).
-- All four ship ENABLED (matching BuiltinToolDefaultEnabled): without
-- file tools an agent cannot do coding work at all, and the blast radius
-- is already bounded by the pod filesystem + workspace jail.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('read', TRUE, '{}'),
        ('write', TRUE, '{}'),
        ('edit', TRUE, '{}'),
        ('apply_patch', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
