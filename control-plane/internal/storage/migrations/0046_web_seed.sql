-- TG-3 (S-249): Seed the system `web` resource and migrate builtin
-- web_fetch enablement into explicit grants. Design: docs/tool-gateway.md
-- §6.1 (denylist model, system floor) + plan TG-3.
--
-- Idempotent: fixed UUIDs + ON CONFLICT DO NOTHING; safe to re-run.

-- ---------------------------------------------------------------------------
-- 1. System service principal owning the seeded platform resources.
--    Fixed UUID ('skquad' in hex, ...0001) so seeds and code agree.
-- ---------------------------------------------------------------------------
INSERT INTO users (id, email, email_verified, name, role)
VALUES (
    '736b7175-6164-4000-8000-000000000001',
    'system@skquad.internal',
    true,
    'skquad system',
    'platform_admin'
)
ON CONFLICT (id) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. System `web` resource. The ceiling mirrors the current BT-6
--    defaults: 256 KiB response cap, no rate limit (absent =
--    unlimited), no private-network reach. Denylists are empty at the
--    floor — admins tighten via endpoint_config; grants may only ADD
--    deny entries (additive-only, enforced by the no-escalation
--    validator at write time and by union-enforcement at call time).
-- ---------------------------------------------------------------------------
INSERT INTO registry_resources (
    id, type, name, description, endpoint, auth_ref, manifest, status,
    registered_by, endpoint_config, policy_ceiling, risk_tier, egress_class
)
VALUES (
    '736b7175-6164-4000-8000-000000000002',
    'web',
    'system-web',
    'Platform-wide governed web fetch (migration from built-in BT-6). Denylist floor: empty; tighten via endpoint_config and per-grant constraints.',
    '', '', '{}', 'active',
    '736b7175-6164-4000-8000-000000000001',
    '{"deny_domains": [], "deny_cidrs": [], "max_bytes": 262144, "timeout_seconds": 30, "allow_private_network": false}',
    '{"deny_domains": [], "deny_cidrs": [], "max_bytes": 262144, "timeout_seconds": 30, "allow_private_network": false}',
    'medium',
    'public'
)
ON CONFLICT (type, name) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 3. Grant migration: every agent that effectively has builtin
--    web_fetch today (platform-level config enabled) gets an explicit
--    grant to the system web resource with empty constraints —
--    reproducing today's behavior explicitly, with an audit-visible
--    grant record. Idempotent via the unique constraint.
-- ---------------------------------------------------------------------------
INSERT INTO agent_permissions (agent_id, resource_type, resource_id, granted_by, constraints)
SELECT
    a.id,
    'web',
    '736b7175-6164-4000-8000-000000000002',
    '736b7175-6164-4000-8000-000000000001',
    '{}'
FROM agents a
WHERE EXISTS (
    SELECT 1
    FROM builtin_tools_config b
    WHERE b.name = 'web_fetch' AND b.enabled
)
ON CONFLICT (agent_id, resource_type, resource_id) DO NOTHING;
