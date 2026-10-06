-- TG-5 slice B2b (S-244-series): MCP upstream drift state.
--
-- Drift detection re-enumerates the upstream tool set through the tool
-- gateway and compares the fresh canonical hash against tools_hash
-- (slice B2a). When the set changed AND a newly-added tool would be
-- permitted by the resource's current tools_allow (only possible via a
-- wildcard entry — exact names were validated against the OLD snapshot),
-- the CP:
--   1. expands the newly-matching wildcard into the concrete OLD-snapshot
--      names it matched, so the gateway's effective allowlist DENIES the
--      added tool immediately (deny-by-rewrite, enforced gateway-side
--      without any gateway change), and
--   2. records the tool name in mcp_drift_pending — the review marker
--      that keeps the tool denied until an admin re-approves via
--      POST /registry/mcp/{id}/approve-tools (which replaces the
--      allowlist wholesale and clears the pending set).
--
--   mcp_drift_pending   JSON array of tool names pending admin
--                      re-approval (NULL/empty = no pending drift)
--   mcp_drift_checked_at  timestamp of the last drift check
--                      (on-demand re-enumerate or hourly scan)
--
-- Design: docs/tool-gateway.md §6.3; slice B2b.

ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS mcp_drift_pending JSONB NULL;
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS mcp_drift_checked_at TIMESTAMPTZ NULL;
