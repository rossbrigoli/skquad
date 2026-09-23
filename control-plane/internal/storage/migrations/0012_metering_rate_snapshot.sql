-- WP5 (ADR-0010 D8 + Risk 3): metering rate snapshot + served-model visibility.
--
-- model_used records the model that ACTUALLY served the call, which can
-- differ from the requested model when the gateway fell back (Risk 3:
-- invisible fallback usage). The rate_* columns snapshot the per-1M
-- pricing used at event time so historical cost never depends on live
-- pricing (D8). rate_snapshot marks rows whose cost was computed from
-- the snapshot rather than passed through from the reporter.
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS model_used text NOT NULL DEFAULT '';
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS rate_input_per_1m double precision;
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS rate_cached_input_per_1m double precision;
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS rate_cache_write_per_1m double precision;
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS rate_output_per_1m double precision;
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS rate_snapshot boolean NOT NULL DEFAULT false;
