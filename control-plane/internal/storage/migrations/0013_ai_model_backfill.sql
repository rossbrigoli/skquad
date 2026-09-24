-- 0013: AI model backfill (ADR-0010, S-113 / WP8 step 2).
-- Additive + idempotent: creates ai_models rows from the deprecated provider
-- metadata (models[], pricing) and binds agents.ai_model_id from the legacy
-- (default_provider, default_model) pair. Nothing is dropped here — the drops
-- live in 0014_drop_legacy_llm_provider.sql and are gated on the step-4
-- cutover running clean (see docs/wp8-migration-plan.md).
--
-- Verified live shapes (dry-run 2026-09-24 13:45 ACST, skquad-system/skquad-postgres-0):
--   providers.models  = plain JSON string array, e.g. ["halogen-qwen3.8-flash-next"]
--   providers.pricing = '{}' on both providers (no real rates to lose)
--   agents.default_provider = uuid (JSON name is default_provider_id; DB column is default_provider)
--   3 agents, 100% resolvable, unresolvable list EMPTY.
--
-- Idempotency: every INSERT is ON CONFLICT DO NOTHING; every UPDATE is
-- ai_model_id IS NULL-guarded. Safe to re-run.

-- ---------------------------------------------------------------------------
-- Step B: create missing ai_models rows from provider metadata.
-- Source models: every entry in providers.models PLUS providers.default_model
-- (free-text legacy default may name a model absent from the array).
-- Pricing mapping (defensive — live data is empty, code never enforced a shape):
--   1. per-model keyed object  {"<model>": {...rates...}} -> use that sub-object
--   2. flat object with *_per_1m keys                   -> apply to all models
--   3. anything else                                    -> '{}' (zero-priced,
--      surfaced to admin via Settings → AI Models, never silently invented)
-- ---------------------------------------------------------------------------
INSERT INTO ai_models (provider_id, display_name, model_name, pricing, status, registered_by)
SELECT
    p.id,
    m.model,
    m.model,
    CASE
        WHEN jsonb_typeof(p.pricing -> m.model) = 'object'
            THEN p.pricing -> m.model
        WHEN jsonb_typeof(p.pricing) = 'object'
             AND (p.pricing ? 'input_per_1m' OR p.pricing ? 'output_per_1m')
            THEN p.pricing
        ELSE '{}'::jsonb
    END,
    'active',
    'wp8-backfill'
FROM providers p
CROSS JOIN LATERAL jsonb_array_elements_text(
    CASE WHEN jsonb_typeof(p.models) = 'array' THEN p.models ELSE '[]'::jsonb END
) AS m(model)
ON CONFLICT (provider_id, model_name) DO NOTHING;

-- providers.default_model may name a model not present in providers.models[].
INSERT INTO ai_models (provider_id, display_name, model_name, pricing, status, registered_by)
SELECT
    p.id,
    p.default_model,
    p.default_model,
    CASE
        WHEN jsonb_typeof(p.pricing -> p.default_model) = 'object'
            THEN p.pricing -> p.default_model
        WHEN jsonb_typeof(p.pricing) = 'object'
             AND (p.pricing ? 'input_per_1m' OR p.pricing ? 'output_per_1m')
            THEN p.pricing
        ELSE '{}'::jsonb
    END,
    'active',
    'wp8-backfill'
FROM providers p
WHERE coalesce(p.default_model, '') <> ''
  AND NOT EXISTS (
      SELECT 1 FROM ai_models am
      WHERE am.provider_id = p.id AND am.model_name = p.default_model
  )
ON CONFLICT (provider_id, model_name) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Step C: bind agents from the legacy (default_provider, default_model) pair.
-- Only agents with NO binding yet. Unresolvable pairs are left unbound (they
-- keep the platform-default path) and reported by the §A4 dry-run query —
-- no silent guesses here either.
-- ---------------------------------------------------------------------------
UPDATE agents a
SET ai_model_id = am.id
FROM ai_models am
WHERE a.ai_model_id IS NULL
  AND coalesce(a.default_model, '') <> ''
  AND am.provider_id = a.default_provider
  AND am.model_name = a.default_model;
