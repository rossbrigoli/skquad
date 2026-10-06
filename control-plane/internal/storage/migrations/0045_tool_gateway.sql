-- TG-2 (S-248): Governed egress plane — typed registry resources, grant
-- constraints, MCP tool snapshots, grant requests, generation-bound
-- credentials. Design: docs/tool-gateway.md §7, plan TG-2.

-- ---------------------------------------------------------------------------
-- 1. Type widening: registry_resources accepts web/rest/mcp/git alongside the
--    legacy set. 'api' stays permitted for compatibility but rows migrate to
--    'rest' below; the storage layer canonicalizes api → rest.
-- ---------------------------------------------------------------------------
ALTER TABLE registry_resources DROP CONSTRAINT IF EXISTS registry_resources_type_check;
ALTER TABLE registry_resources ADD CONSTRAINT registry_resources_type_check CHECK (
    type IN ('skill','tool','api','rest','web','mcp','git','knowledge_base','project_workspace')
);

-- ---------------------------------------------------------------------------
-- 2. Typed config + policy ceiling + tier/class/owner.
--    endpoint_config: per-type connection shape (validated server-side; never
--    contains secret material — secrets stay in auth_ref → K8s Secret).
--    policy_ceiling: the maximum a grant to this resource may request.
-- ---------------------------------------------------------------------------
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS endpoint_config JSONB NOT NULL DEFAULT '{}';
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS policy_ceiling JSONB NOT NULL DEFAULT '{}';
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS risk_tier TEXT NOT NULL DEFAULT 'low'
    CHECK (risk_tier IN ('low','medium','high'));
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS egress_class TEXT NOT NULL DEFAULT 'public'
    CHECK (egress_class IN ('public','internal'));
-- Resource-owner attribute (§8: an attribute, not an RBAC role). Nullable:
-- legacy and system resources may have no explicit owner.
ALTER TABLE registry_resources ADD COLUMN IF NOT EXISTS owner_user_id UUID NULL REFERENCES users(id);
CREATE INDEX IF NOT EXISTS idx_registry_resources_owner ON registry_resources(owner_user_id);

-- ---------------------------------------------------------------------------
-- 3. Migrate legacy 'api' rows to 'rest' (type widening, §7 note 1).
--    The compatibility alias is code-side (storage canonicalizes 'api' →
--    'rest' on every read/write path) plus this view for any SQL consumer
--    that still queries the old type value.
-- ---------------------------------------------------------------------------
UPDATE registry_resources SET type = 'rest' WHERE type = 'api';

CREATE OR REPLACE VIEW registry_resources_api_alias AS
    SELECT * FROM registry_resources WHERE type = 'rest';

-- ---------------------------------------------------------------------------
-- 4. Grant constraints: agent_permissions is the agent-grant table. A
--    grant's constraints must be ⊆ the resource's policy_ceiling
--    (no-escalation invariant, validated in the application layer).
-- ---------------------------------------------------------------------------
ALTER TABLE agent_permissions DROP CONSTRAINT IF EXISTS agent_permissions_resource_type_check;
ALTER TABLE agent_permissions ADD CONSTRAINT agent_permissions_resource_type_check CHECK (
    resource_type IN ('llm_provider','skill','tool','api','rest','web','mcp','git','knowledge_base','project_workspace')
);
ALTER TABLE agent_permissions ADD COLUMN IF NOT EXISTS constraints JSONB NOT NULL DEFAULT '{}';

UPDATE agent_permissions SET resource_type = 'rest' WHERE resource_type = 'api';

-- ---------------------------------------------------------------------------
-- 5. MCP tool snapshots (drift detection, §6.3). One row per capture;
--    the newest row is authoritative. review_state gates usage:
--    approved → usable; drift_detected → new/changed tools denied until
--    admin review; suspended → resource unusable via gateway.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS mcp_tool_snapshots (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id   UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    snapshot_hash TEXT NOT NULL,
    tools         JSONB NOT NULL,
    captured_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    review_state  TEXT NOT NULL DEFAULT 'approved'
        CHECK (review_state IN ('approved','drift_detected','suspended'))
);
CREATE INDEX IF NOT EXISTS idx_mcp_tool_snapshots_resource ON mcp_tool_snapshots(resource_id, captured_at DESC);

-- ---------------------------------------------------------------------------
-- 6. Grant requests (approval workflow schema only — TG-8 implements the
--    state machine). Records who asked for what under which constraints.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS grant_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    resource_id UUID NOT NULL REFERENCES registry_resources(id) ON DELETE CASCADE,
    requested_by UUID NOT NULL REFERENCES users(id),
    grantee_type TEXT NOT NULL CHECK (grantee_type IN ('user','agent')),
    grantee_id UUID NOT NULL,
    constraints JSONB NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'pending_owner'
        CHECK (status IN ('pending_owner','pending_admin','approved','denied')),
    owner_decision_at TIMESTAMPTZ NULL,
    admin_decision_at TIMESTAMPTZ NULL,
    notes TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_grant_requests_resource ON grant_requests(resource_id);
CREATE INDEX IF NOT EXISTS idx_grant_requests_status ON grant_requests(status);

-- ---------------------------------------------------------------------------
-- 7. Generation-bound credentials (§5.2, OpenShell-adopted). Each agent
--    credential generation is a monotonic counter on the identity row;
--    rotation increments it. Old-generation tokens already stop verifying
--    because rotation replaces the verifier — the generation makes the
--    epoch visible to the gateway/policy API without redesigning identity.
-- ---------------------------------------------------------------------------
ALTER TABLE agent_identities ADD COLUMN IF NOT EXISTS generation INTEGER NOT NULL DEFAULT 1
    CHECK (generation >= 1);
