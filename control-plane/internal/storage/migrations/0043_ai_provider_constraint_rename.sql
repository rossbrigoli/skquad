-- 0043 (S-231): "LLM Provider" concept renamed to "AI Provider".
--
-- The table itself was already renamed llm_providers -> providers back in 0011,
-- but upgraded databases still carry the constraint/index names inherited from
-- the original llm_providers table (Postgres keeps implicit names across
-- ALTER TABLE ... RENAME TO). Fresh installs (which create `providers`
-- directly in 0011) already have the providers_* names.
--
-- This migration renames the leftover implicit objects so no "llm" remains on
-- the provider entity. Pure metadata renames — ALTER ... RENAME preserves all
-- data and does not rebuild indexes. Every rename is guarded so the migration
-- is idempotent and a no-op on fresh installs.
DO $$
BEGIN
    IF to_regclass('public.providers') IS NULL THEN
        RETURN;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'public.providers'::regclass
                 AND conname = 'llm_providers_pkey') THEN
        ALTER TABLE providers RENAME CONSTRAINT llm_providers_pkey TO providers_pkey;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'public.providers'::regclass
                 AND conname = 'llm_providers_name_key') THEN
        ALTER TABLE providers RENAME CONSTRAINT llm_providers_name_key TO providers_name_key;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'public.providers'::regclass
                 AND conname = 'llm_providers_registered_by_fkey') THEN
        ALTER TABLE providers RENAME CONSTRAINT llm_providers_registered_by_fkey TO providers_registered_by_fkey;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_constraint
               WHERE conrelid = 'public.providers'::regclass
                 AND conname = 'llm_providers_status_check') THEN
        ALTER TABLE providers RENAME CONSTRAINT llm_providers_status_check TO providers_status_check;
    END IF;
END
$$;
