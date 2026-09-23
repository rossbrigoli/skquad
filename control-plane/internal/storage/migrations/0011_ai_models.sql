-- 0011: AI model registry + user grants (ADR-0010, S-106).
-- `llm_providers` is demoted and renamed to `providers`: an INTERNAL credential
-- holder (one base_url + api_key_ref shared by N models). The grantable unit is
-- now `ai_models`, granted to `users` (user_model_grants) and bound 1:1 to
-- agents as primary + optional fallback. Model metadata (models[], default_model,
-- pricing) moves out of the credential holder into ai_models.

-- ---------------------------------------------------------------------------
-- Rename llm_providers -> providers (idempotent: skip when already renamed).
-- ---------------------------------------------------------------------------
DO $$
BEGIN
    IF to_regclass('public.llm_providers') IS NOT NULL
       AND to_regclass('public.providers') IS NULL THEN
        ALTER TABLE llm_providers RENAME TO providers;
    END IF;
END
$$;

-- Fresh-install fallback: create the credential holder directly when neither
-- the legacy nor the new table exists yet.
CREATE TABLE IF NOT EXISTS providers (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL UNIQUE,
    kind          text NOT NULL,
    base_url      text NOT NULL,
    api_key_ref   text NOT NULL,
    status        text NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','deprecated')),
    registered_by uuid NOT NULL REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Model metadata no longer lives on the credential holder (it moved to
-- ai_models; see ADR-0010 D2).
ALTER TABLE providers DROP COLUMN IF EXISTS models;
ALTER TABLE providers DROP COLUMN IF EXISTS default_model;
ALTER TABLE providers DROP COLUMN IF EXISTS pricing;

-- ---------------------------------------------------------------------------
-- AI models: the admin-registered, grantable unit (ADR-0010 D1).
-- Pricing holds the four per-1M rates (input, cached_input, cache_write,
-- output) plus tiering metadata; cost is snapshotted at metering time (D8).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ai_models (
    id                           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    provider_id                  uuid NOT NULL REFERENCES providers(id) ON DELETE RESTRICT,
    display_name                 text NOT NULL,
    model_name                   text NOT NULL,
    context_window               integer NOT NULL DEFAULT 0,
    supports_tools               boolean NOT NULL DEFAULT false,
    pricing                      jsonb NOT NULL DEFAULT '{}'::jsonb,
    long_context_threshold_tokens integer NOT NULL DEFAULT 0,
    status                       text NOT NULL DEFAULT 'active'
                                 CHECK (status IN ('active','deprecated')),
    registered_by               text NOT NULL DEFAULT '',
    created_at                   timestamptz NOT NULL DEFAULT now(),
    updated_at                   timestamptz,
    UNIQUE (provider_id, model_name)
);
CREATE INDEX IF NOT EXISTS idx_ai_models_status ON ai_models(status);

-- ---------------------------------------------------------------------------
-- User -> AI model grants (ADR-0010 D3: grants follow people, not squads).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_model_grants (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    grantee_user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ai_model_id     uuid NOT NULL REFERENCES ai_models(id) ON DELETE CASCADE,
    granted_by      uuid NOT NULL REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (grantee_user_id, ai_model_id)
);
CREATE INDEX IF NOT EXISTS idx_user_model_grants_model ON user_model_grants(ai_model_id);

-- ---------------------------------------------------------------------------
-- Agent binding: exactly one primary + optional one fallback (ADR-0010 D4).
-- NOTE: ai_model_id stays NULLABLE in this WP; NOT NULL is added in WP8 only
-- after the backfill reaches 100% of live agents.
-- ---------------------------------------------------------------------------
ALTER TABLE agents ADD COLUMN IF NOT EXISTS ai_model_id uuid REFERENCES ai_models(id);
ALTER TABLE agents ADD COLUMN IF NOT EXISTS fallback_ai_model_id uuid REFERENCES ai_models(id);
ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_fallback_nequals_primary;
ALTER TABLE agents ADD CONSTRAINT agents_fallback_nequals_primary
    CHECK (fallback_ai_model_id IS NULL OR fallback_ai_model_id <> ai_model_id);
