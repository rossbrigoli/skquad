-- 0014: Drop the legacy llm_provider grant type and the legacy
-- provider/agent default columns (WP8 closeout, ADR-0010).
--
-- Prerequisites (all verified live before authoring, 2026-09-25):
--   * 0013_ai_model_backfill.sql applied 2026-09-24; all 3 agents bound
--     to ai_models (agents.ai_model_id), data gates V1-V3 = 0.
--   * providers.pricing = '{}' everywhere (S-128 already removed
--     provider-level pricing from the JSON surface; the column was
--     unread/unwritten).
--   * Exactly 1 legacy row remains: agent_permissions WHERE
--     resource_type = 'llm_provider' — deleted here.
--
-- The control-plane code no longer reads or writes any of the columns
-- dropped below (WP8 step-4 cutover shipped in c534b85's successor).
--
-- Idempotency: every statement is IF EXISTS / catalog-guarded, matching
-- the 0013 style. The runner wraps this file in a transaction and never
-- re-applies it once recorded, but a manual re-run must be a safe no-op.

-- ---------------------------------------------------------------------------
-- (a) Remove the legacy llm_provider grants. Must precede the CHECK
--     replacement in (b): ADD CONSTRAINT validates existing rows.
-- ---------------------------------------------------------------------------
DELETE FROM agent_permissions WHERE resource_type = 'llm_provider';

-- ---------------------------------------------------------------------------
-- (b) Replace the resource_type CHECK without 'llm_provider'.
-- 0001_init.sql created the constraint inline (anonymous), so Postgres
-- auto-named it agent_permissions_resource_type_check. Allowed set is the
-- identical 0001 set minus 'llm_provider':
--   ('skill','tool','api','knowledge_base','project_workspace')
-- which matches registry_resources.type (0001) exactly.
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'agent_permissions'::regclass
          AND conname  = 'agent_permissions_resource_type_check'
    ) THEN
        ALTER TABLE agent_permissions
            DROP CONSTRAINT agent_permissions_resource_type_check;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'agent_permissions'::regclass
          AND conname  = 'agent_permissions_resource_type_check'
    ) THEN
        ALTER TABLE agent_permissions
            ADD CONSTRAINT agent_permissions_resource_type_check
            CHECK (resource_type IN
                ('skill','tool','api','knowledge_base','project_workspace'));
    END IF;
END
$$;

-- ---------------------------------------------------------------------------
-- (c) Drop the legacy columns.
--   agents.default_provider / default_model: superseded by
--     agents.ai_model_id / fallback_ai_model_id (ADR-0010 D4), backfilled
--     by 0013. default_provider carried an FK to the old llm_providers
--     table; DROP COLUMN removes the dependent constraint with it.
--   providers.default_model / models / pricing: superseded by ai_models
--     (ADR-0010 D2/D8); 0011 explicitly deferred these drops to here.
-- ---------------------------------------------------------------------------
ALTER TABLE agents    DROP COLUMN IF EXISTS default_provider;
ALTER TABLE agents    DROP COLUMN IF EXISTS default_model;
ALTER TABLE providers DROP COLUMN IF EXISTS default_model;
ALTER TABLE providers DROP COLUMN IF EXISTS models;
ALTER TABLE providers DROP COLUMN IF EXISTS pricing;
