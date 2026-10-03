package domain

import "time"

// S-203 WP1: Budget model for cost management.
//
// Budgets are per-user monthly limits in USD. The platform-level knobs
// (default, max, platform-wide limit) live in the existing
// platform_settings key/value store so they need no dedicated table and
// follow the S-183 pattern; per-user budgets live in user_budgets
// (migration 0036).
//
// Semantics:
//   - budget_default_usd: auto-applied to newly created users on first
//     login (EnsureUserDefaultBudget). Unset ⇒ new users get no budget.
//   - budget_max_usd: hard ceiling for every user budget. Lowering it
//     clamps ALL user budgets above the new max in the same operation
//     (epic S-203 req 7).
//   - budget_platform_monthly_limit_usd: optional platform-wide monthly
//     spend cap across all users. Unset (null) ⇒ no platform limit.
const (
	PlatformSettingBudgetDefaultUSD              = "budget_default_usd"
	PlatformSettingBudgetMaxUSD                  = "budget_max_usd"
	PlatformSettingBudgetPlatformMonthlyLimitUSD = "budget_platform_monthly_limit_usd"
)

// UserBudget is the per-user monthly budget row.
type UserBudget struct {
	UserID           string    `json:"user_id"`
	MonthlyBudgetUSD float64   `json:"monthly_budget_usd"`
	UpdatedAt        time.Time `json:"updated_at"`
	UpdatedBy        string    `json:"updated_by,omitempty"`
}

// PlatformBudgets is the platform-level budget configuration. A nil
// pointer means "unset" (no default / no max / no platform limit).
type PlatformBudgets struct {
	DefaultMonthlyUSD       *float64 `json:"default_monthly_usd"`
	MaxUSD                  *float64 `json:"max_usd"`
	PlatformMonthlyLimitUSD *float64 `json:"platform_monthly_limit_usd"`
}
