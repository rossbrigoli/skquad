-- 0035: S-212 memory_search builtin tool.
--
-- Widens the builtin_tools_config name CHECK to admit 'memory_search'
-- and seeds it ENABLED. Like send_message (0023), the blast radius is
-- bounded: recall is hard-scoped to the calling agent's own
-- agent_memory rows (agent_id = principal), rejected memories are
-- excluded, and the whole path is read-only. The feature additionally
-- no-ops server-side unless SKQUAD_MEMORY_EMBEDDINGS_ENABLED=true.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search'));

INSERT INTO builtin_tools_config (name, enabled) VALUES ('memory_search', TRUE)
    ON CONFLICT (name) DO NOTHING;
