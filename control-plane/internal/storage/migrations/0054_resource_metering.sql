-- S-269: per-resource metering (vertical slice).
--
-- Registry resources carry an OPTIONAL metering config (Ross's hard
-- requirement): metered=false (the default) means every governed call
-- to the resource costs 0 — an explicit billed zero, not missing data.
-- metered=true bills price_per_call per call; the API boundary
-- validates that a metered resource has a price >= 0. Currency
-- defaults to the platform currency (USD, matching the metering table
-- default).
--
-- The metering table gains an optional resource dimension. Usage (call
-- counts) is recorded for ALL resources, metered or not; cost is
-- charged only when metered (cost = price_per_call per call, fixed at
-- event time so later price edits never rewrite history — same
-- event-time snapshot philosophy as the WP5 rate snapshot).
-- LLM-token attribution per resource is OUT of scope for this slice.
--
-- Retention/aggregation (S-203 alignment): per-resource rollups follow
-- the existing daily-rollup pattern (SumMeteringDaily) — raw events
-- are retained under the existing metering retention policy and the
-- rollup is computed at read time; no separate rollup table or new
-- retention stream is introduced.

ALTER TABLE IF EXISTS registry_resources ADD COLUMN IF NOT EXISTS metered boolean NOT NULL DEFAULT false;
ALTER TABLE IF EXISTS registry_resources ADD COLUMN IF NOT EXISTS price_per_call double precision;
ALTER TABLE IF EXISTS registry_resources ADD COLUMN IF NOT EXISTS currency text NOT NULL DEFAULT 'USD';

ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS resource_id uuid;
ALTER TABLE IF EXISTS metering ADD COLUMN IF NOT EXISTS resource_type text NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_metering_resource ON metering (resource_id, timestamp);
