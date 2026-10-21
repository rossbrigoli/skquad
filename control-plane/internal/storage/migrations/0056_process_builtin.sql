-- 0056: S-280 background process management builtin (process).
--
-- Widens the builtin_tools_config name CHECK to admit the PROCESS tool
-- and seeds it ENABLED. Paired with exec's background/yieldMs arguments,
-- it gives agents OpenClaw-style control over detached commands in
-- their task workspace: list, poll, log, write (stdin), kill (whole
-- process group via killpg) and clear. The security boundary is
-- runtime-side: sessions are tracked in a per-workspace registry under
-- the task dir, logs stay inside the workspace jail, and the kill path
-- uses os.killpg so child processes die too. Ships ENABLED (matching
-- BuiltinToolDefaultEnabled): background exec is useless without the
-- control half, and the blast radius is bounded by the pod filesystem,
-- the session cap and the per-session log cap.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('process', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
