-- S-309: bulk memory import (owner-authenticated migration of curated
-- operator memory into agent_memory).
--
-- 1. New trust level 'trusted_user': content supplied directly by the
--    owning user (not model output), imported via
--    POST /api/v1/agents/{id}/memory/import with review_status
--    'approved' and provenance 'migrated'.
-- 2. Partial unique index backing import idempotency: re-sending the
--    same (agent, batch_id, content sha256) triple cannot create a
--    duplicate row. Scoped to rows that carry BOTH metadata keys so
--    non-import memory rows (which lack them) are unaffected.
ALTER TABLE agent_memory
    DROP CONSTRAINT IF EXISTS agent_memory_trust_level_check,
    ADD CONSTRAINT agent_memory_trust_level_check
        CHECK (trust_level IN ('raw_model_output', 'distilled', 'verified', 'trusted_user'));

CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_import_idempotency
    ON agent_memory (agent_id, (metadata->>'batch_id'), (metadata->>'content_sha256'))
    WHERE metadata ? 'batch_id' AND metadata ? 'content_sha256';
