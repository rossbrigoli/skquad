-- 0036: Budget model for S-203 cost management (WP1).
--
-- Per-user monthly budgets in USD. Platform-level knobs (default budget,
-- global max, platform-wide monthly limit) live in the existing
-- platform_settings key/value store (migration 0026) under
-- budget_default_usd / budget_max_usd / budget_platform_monthly_limit_usd
-- and are intentionally NOT seeded here: unset means "no default / no
-- max / no platform limit".
--
-- Invariants enforced at the API boundary (control-plane budgets.go):
--   * user budgets are >= 0 and <= budget_max_usd,
--   * the default is <= budget_max_usd,
--   * lowering budget_max_usd clamps every user budget above it in the
--     same request (epic S-203 req 7).
--
-- Idempotency: IF NOT EXISTS, matching the 0016+ style.

CREATE TABLE IF NOT EXISTS user_budgets (
    user_id             uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    monthly_budget_usd  numeric(12,2) NOT NULL CHECK (monthly_budget_usd >= 0),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_user_budgets_amount ON user_budgets(monthly_budget_usd);
