-- TG-5 slice B2a (S-244-series): MCP tool snapshot columns.
--
-- Registration enumerates the live tool set through the tool gateway
-- (POST /internal/mcp/enumerate) and the control-plane stores the snapshot:
--   tools_snapshot      JSON array of {name, description, inputSchema} as
--                     returned by the gateway's tools/list
--   tools_hash          the gateway-computed canonical sha256 (64-hex),
--                     stored verbatim — the CP never recomputes it
--   tools_enumerated_at timestamp of the last successful enumeration
--
-- Drift detection (slice B2b) re-enumerates and compares the fresh hash
-- against tools_hash; a mismatch means the upstream tool set changed and
-- admin review is required (new/changed tools stay denied by default).
-- Design: docs/tool-gateway.md §6.3.

ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS tools_snapshot JSONB NULL;
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS tools_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS tools_enumerated_at TIMESTAMPTZ NULL;
