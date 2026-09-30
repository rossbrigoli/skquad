-- 0026: Platform settings key/value store (S-183 idle scale-to-zero).
--
-- platform_settings holds platform-admin configuration as strings so
-- new settings land without schema changes; shape/range validation lives
-- at the API boundary. Seeded with the S-183 default: agents scale to
-- zero only after 15 minutes (900s) of no activity.
--
-- The old behaviour baked the deploy-time default (300s) into every
-- agent row at creation. Agents still carrying exactly that legacy
-- default are reset to 0, which now means "follow the platform
-- setting" (per-agent values > 0 remain explicit overrides).
--
-- Idempotency: IF NOT EXISTS / ON CONFLICT, matching the 0016+ style.

CREATE TABLE IF NOT EXISTS platform_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by TEXT NOT NULL DEFAULT ''
);

INSERT INTO platform_settings (key, value)
VALUES ('idle_scale_to_zero_seconds', '900')
ON CONFLICT (key) DO NOTHING;

UPDATE agents SET idle_timeout_sec = 0 WHERE idle_timeout_sec = 300;
