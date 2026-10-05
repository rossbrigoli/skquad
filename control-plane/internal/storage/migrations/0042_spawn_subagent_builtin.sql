-- 0042: S-232 spawn_subagent builtin tool.
--
-- Widens the builtin_tools_config name CHECK to admit 'spawn_subagent'
-- and seeds it ENABLED. The capability is runtime-provided (S-160/S-163:
-- a nested agent loop that inherits the parent's model, grants, config
-- and composed prompt, with recursion excluded), so it was never part of
-- the control-plane builtin catalog and thus invisible on the Settings
-- Tools page. Listing it here makes it a first-class builtin: admins see
-- it, and it cannot be deleted (PRIMARY KEY row + no delete route).
-- The runtime additionally gates the capability on SKQUAD_SUBAGENTS_ENABLED;
-- the policy carries no admin-tunable keys yet ('{}' only).
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES ('spawn_subagent', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
