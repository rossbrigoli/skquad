-- 0023: send_message builtin tool (S-164, agent-to-agent messaging).
--
-- Widens the builtin_tools_config name CHECK to admit 'send_message' and
-- seeds it ENABLED. Unlike the original three builtins (disabled by
-- default per ADR-0012 §1), intra-squad messaging is the core squad
-- primitive: blast radius is bounded by squad isolation, control-plane
-- grant checks for cross-squad sends, and the correlation-chain budget
-- (max 12 messages per thread) that fails runaway reply loops loudly.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message'));

INSERT INTO builtin_tools_config (name, enabled) VALUES ('send_message', TRUE)
    ON CONFLICT (name) DO NOTHING;
