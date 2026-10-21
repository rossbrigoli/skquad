-- 0059: S-283 built-in BROWSER tool (ADR-0014 Browser-as-a-Service).
--
-- The browser builtin is a thin client over the Tool Gateway's browser
-- driver: the runtime dispatches MCP tool calls (browser.navigate,
-- browser.click, browser.type, browser.screenshot, browser.extract,
-- browser.close_session) to the quarantined browser-service via
-- POST {gateway}/v1/mcp/{resourceID}/call. The agent never holds a
-- browser session token and never talks to the service directly —
-- session brokering, navigation ceilings (deny_hosts, max_pages) and
-- the egress quarantine are enforced gateway/service-side
-- (tool-gateway/internal/drivers/browser/, browser-service/).
--
-- The tool is inert unless the agent holds a granted mcp resource whose
-- endpoint_config declares `driver: "browser"` AND the runtime has
-- SKQUAD_TOOL_GATEWAY_URL set (both fail-closed runtime-side; the
-- gateway denies without a grant). Default-enablement therefore cannot
-- widen reach.
--
-- Widen the builtin_tools_config name CHECK to admit the tool (same
-- pattern as 0055/0056/0057/0058) and seed it ENABLED (mirrors
-- domain.BuiltinToolDefaultEnabled). Policy keys are runtime-side
-- bounds only (timeoutSeconds, maxSnapshotChars, maxScreenshotBytes);
-- the security-relevant ceilings live on the resource grant's ceiling.
ALTER TABLE builtin_tools_config
    DROP CONSTRAINT IF EXISTS builtin_tools_config_name_check;

ALTER TABLE builtin_tools_config
    ADD CONSTRAINT builtin_tools_config_name_check
    CHECK (name IN ('exec','web_fetch','web_search','send_message','send_inbox','notify_owner','memory_search','spawn_subagent','read','write','edit','apply_patch','process','cron','sessions_list','sessions_history','session_status','browser'));

INSERT INTO builtin_tools_config (name, enabled, policy)
    VALUES
        ('browser', TRUE, '{}')
    ON CONFLICT (name) DO NOTHING;
