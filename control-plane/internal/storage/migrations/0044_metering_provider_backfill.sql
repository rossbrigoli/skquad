-- 0044 (S-238): backfill metering.provider_id from the AI model registry.
--
-- The gateway metering callback historically omitted provider_id, so
-- metering rows accumulated with no provider linkage (0 of 774 live rows
-- carried one). Per-provider cost rollups only rendered because
-- SumMeteringDaily resolves the provider at query time via the
-- ai_models registry join (S-230). This migration makes the linkage
-- durable: every metering row whose model resolves to a registered AI
-- model gets its provider_id stamped in, so the data is correct at rest
-- and the query-time join stays a fallback rather than the only link.
--
-- Resolution mirrors the ingest rule (S-238) and the S-230 query join:
-- the SERVED model (model_used, falling back to model) names the
-- registered ai_models row. Deterministic pick on provider_id when a
-- model_name exists under multiple providers (never multiplies rows).
--
-- Rows whose model cannot be resolved are deliberately LEFT unlinked —
-- they keep rendering under the "unknown provider" label instead of a
-- guessed provider. No fabrication.
--
-- Idempotency: the UPDATE only touches rows with NULL provider_id; a
-- second run matches nothing new. Safe to re-run.
UPDATE metering m
SET provider_id = resolved.provider_id
FROM (
    SELECT m2.id AS metering_id,
           (SELECT am.provider_id
            FROM ai_models am
            WHERE am.model_name = coalesce(nullif(m2.model_used, ''), m2.model)
            ORDER BY am.provider_id
            LIMIT 1) AS provider_id
    FROM metering m2
    WHERE m2.provider_id IS NULL
) resolved
WHERE m.id = resolved.metering_id
  AND resolved.provider_id IS NOT NULL;
