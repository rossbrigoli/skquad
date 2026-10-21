-- S-283 follow-on (browser activation): seed the platform-managed
-- browser MCP resource and grant it to every agent so the shipped
-- `browser` builtin (agent-runtime builtin_tools.py) discovers it and
-- the gateway's TG-6 browser driver can broker sessions.
--
-- Registration shape mirrors the TG-6 slice D contract
-- (control-plane/internal/egresspolicy/browser.go +
-- tool-gateway/internal/drivers/browser/{driver,policy}.go):
--   * mcp-typed resource; endpoint_config.driver = "browser" selects
--     the browser driver at the gateway (grantRequestsBrowserDriver).
--   * egress_class = "internal" — enforced for browser-driver mcp
--     resources by validateTypedResourceFields (in-cluster service).
--   * policy_ceiling is the FLAT browser shape (browserPolicyShape),
--     fully materialized with the protocol defaults from
--     docs/tg6-browser-protocol.md §2/§6. deny_hosts follows the
--     canonical seed set from the protocol doc, plus *.metadata.goog
--     and the link-local metadata IP for belt-and-braces; the service
--     structurally blocks RFC1918/link-local/CGNAT/ULA anyway
--     (browser-service/src/enforce.ts isPrivateIp), so no CIDRs are
--     needed here (deny_hosts is host-glob matched).
--   * auth_ref points at the chart-rendered managed Secret
--     (skquad-mcp-<id> in the release namespace, bearer key "token"
--     per the custody contract in httpapi/rest_credentials.go /
--     kube.MCPSecretName). NOTE: the browser driver itself
--     authenticates to the service with the gateway's
--     SKQUAD_BROWSER_INTERNAL_TOKEN env (already chart-wired); the
--     managed Secret keeps the registration shape consistent with
--     the bearer custody pattern and serves the same material via
--     /internal/v1/credentials if ever resolved.
--
-- Grants: agent_permissions IS the gateway grant table
-- (buildPolicySnapshot → ListAgentPermissions → policyGrant.Config/
-- Ceiling/Constraints; standing_grants is the TG-8 confirmation-skip
-- table and is NOT on the grant path — seeding it would pre-authorize
-- confirmation-gated tools without human approval, so it is
-- deliberately left untouched).
--
-- Idempotent: fixed UUIDs + ON CONFLICT DO NOTHING; safe to re-run.

-- ---------------------------------------------------------------------------
-- 1. Platform browser resource (mcp-typed, browser driver).
-- ---------------------------------------------------------------------------
INSERT INTO registry_resources (
    id, type, name, description, endpoint, auth_ref, manifest, status,
    registered_by, endpoint_config, policy_ceiling, risk_tier, egress_class
)
VALUES (
    '736b7175-6164-4000-8000-0000000000b0',
    'mcp',
    'platform-browser',
    'Platform-managed headless browser (TG-6 browser-as-a-service). Governed via the gateway browser driver: session brokering, navigation ceiling, screenshot caps.',
    'http://skquad-browser-service.skquad-browser.svc.cluster.local:8090/mcp',
    'k8s://skquad-system/skquad-mcp-736b7175-6164-4000-8000-0000000000b0',
    '{}', 'active',
    '736b7175-6164-4000-8000-000000000001',
    '{"url": "http://skquad-browser-service.skquad-browser.svc.cluster.local:8090/mcp", "auth_kind": "bearer", "driver": "browser"}',
    '{"deny_hosts": ["*.internal", "metadata.google.internal", "*.metadata.goog", "169.254.169.254"], "max_pages": 50, "max_screenshot_bytes": 2097152, "idle_timeout_s": 600, "max_session_minutes": 30, "max_sessions_per_agent": 1}',
    'high',
    'internal'
)
ON CONFLICT (type, name) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Grant the platform browser to every agent with empty constraints
--    (agents get exactly the ceiling; per-agent tightening is done by
--    updating the grant's constraints later). Idempotent via the
--    unique (agent_id, resource_type, resource_id) constraint.
-- ---------------------------------------------------------------------------
INSERT INTO agent_permissions (agent_id, resource_type, resource_id, granted_by, constraints)
SELECT
    a.id,
    'mcp',
    '736b7175-6164-4000-8000-0000000000b0',
    '736b7175-6164-4000-8000-000000000001',
    '{}'
FROM agents a
ON CONFLICT (agent_id, resource_type, resource_id) DO NOTHING;
