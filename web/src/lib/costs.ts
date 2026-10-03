// S-224 (S-203 WP2): Cost Management page logic — pure, browser-
// independent helpers + API response types mirroring the WP1 control-plane
// contracts (control-plane/internal/httpapi/budgets.go, squash 9945941).
//
// Endpoints consumed:
//   GET  /api/v1/costs/summary?days=N        — scoped daily series + caller budget
//   GET  /api/v1/admin/budgets               — admin overview (platform + users)
//   PUT  /api/v1/admin/budgets/platform      — default / max / platform limit
//   PUT  /api/v1/admin/budgets/users/{id}    — per-user monthly budget
//
// The backend already enforces scoping (admins see everything, users only
// their squads/agents/models/providers) — the UI renders what it gets and
// only decides which TABS to show.

import type { UsageSeries } from "./usage";

// ---------------------------------------------------------------------------
// API response types (snake_case mirrors the Go JSON)
// ---------------------------------------------------------------------------

export type CostBudgetStatus = {
  user_id: string;
  email?: string;
  name?: string;
  monthly_budget_usd: number | null;
  mtd_cost: number;
  remaining_usd: number | null;
  over_budget: boolean;
  updated_at?: string;
  updated_by?: string;
};

export type CostSummaryPayload = {
  scope: string;
  currency?: string;
  days: string[];
  mtd_start: string;
  total_cost: number;
  mtd_cost: number;
  running_cost: number;
  by_squad: UsageSeries[];
  by_agent: UsageSeries[];
  by_model: UsageSeries[];
  by_provider: UsageSeries[];
  budget?: CostBudgetStatus | null;
};

export type PlatformBudget = {
  default_monthly_usd: number | null;
  max_usd: number | null;
  platform_monthly_limit_usd: number | null;
  platform_mtd_cost: number;
};

export type AdminBudgetsPayload = {
  platform: PlatformBudget;
  users: CostBudgetStatus[];
};

// PUT /admin/budgets/platform response: the knobs after the write plus
// clamped_user_budgets (count of user budgets pulled down to the new max)
// when the max was lowered.
export type PlatformBudgetPutResponse = {
  default_monthly_usd: number | null;
  max_usd: number | null;
  platform_monthly_limit_usd: number | null;
  clamped_user_budgets?: number;
};

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

export type CostTab = "cost" | "budget";

// resolveCostTab clamps the requested tab to what the caller may see:
// the Budget tab is platform-admin-only. Non-admins always fall back to
// the Cost tab (mirrors settings' resolveSettingsTab pattern).
export function resolveCostTab(tab: CostTab, isAdmin: boolean): CostTab {
  if (tab === "budget" && !isAdmin) return "cost";
  return tab;
}

// ---------------------------------------------------------------------------
// Chart grouping
// ---------------------------------------------------------------------------

// CostGroupSource picks which grouping the daily cost chart renders.
// The card requires squad, agent, model and provider breakdowns; a
// segmented control swaps the series set without a page reload.
export type CostGroupSource = "squads" | "agents" | "models" | "providers";

export const COST_GROUP_SOURCES: { id: CostGroupSource; label: string }[] = [
  { id: "squads", label: "Squads" },
  { id: "agents", label: "Agents" },
  { id: "models", label: "Models" },
  { id: "providers", label: "Providers" },
];

export function seriesForSource(payload: CostSummaryPayload | null, source: CostGroupSource): UsageSeries[] {
  if (!payload) return [];
  switch (source) {
    case "squads":
      return payload.by_squad ?? [];
    case "agents":
      return payload.by_agent ?? [];
    case "models":
      return payload.by_model ?? [];
    case "providers":
      return payload.by_provider ?? [];
  }
}

// ---------------------------------------------------------------------------
// Budget bar
// ---------------------------------------------------------------------------

export type BudgetBarModel = {
  hasBudget: boolean;
  spend: number;
  budget: number;
  // fraction of the budget consumed, clamped to [0, 1] for bar width.
  fraction: number;
  // true when spend exceeds the budget (bar renders over-budget).
  over: boolean;
  // remaining dollars; 0 when over budget.
  remaining: number;
};

// buildBudgetModel folds spend + budget into the horizontal bar model.
// A null/negative-or-missing budget means "no limit" → hasBudget false.
export function buildBudgetModel(spend: number, budget: number | null | undefined): BudgetBarModel {
  const s = Number.isFinite(spend) ? spend : 0;
  if (budget == null || budget <= 0) {
    return { hasBudget: false, spend: s, budget: 0, fraction: 0, over: false, remaining: 0 };
  }
  const over = s > budget;
  return {
    hasBudget: true,
    spend: s,
    budget,
    fraction: Math.min(1, Math.max(0, s / budget)),
    over,
    remaining: over ? 0 : budget - s,
  };
}

// ---------------------------------------------------------------------------
// Budget form parsing
// ---------------------------------------------------------------------------

// parseBudgetInput turns a form field into a knob value:
//   ""            → { clear: true }   (explicit null — no limit)
//   valid number  → { value: n }
//   anything else → { error: msg }
// Negative values and non-numbers are rejected client-side; the API
// repeats the check server-side.
export type BudgetInputResult =
  | { kind: "clear" }
  | { kind: "value"; value: number }
  | { kind: "error"; message: string };

export const MAX_BUDGET_USD = 1_000_000_000;

export function parseBudgetInput(raw: string): BudgetInputResult {
  const trimmed = (raw ?? "").trim();
  if (trimmed === "") return { kind: "clear" };
  const n = Number(trimmed);
  if (!Number.isFinite(n)) return { kind: "error", message: "must be a number or left empty for no limit" };
  if (n < 0) return { kind: "error", message: "must not be negative" };
  if (n > MAX_BUDGET_USD) return { kind: "error", message: "exceeds the accepted maximum" };
  return { kind: "value", value: n };
}

// knobToInput renders a stored knob back into a form field value
// (null/undefined → empty string = no limit).
export function knobToInput(value: number | null | undefined): string {
  return value == null ? "" : String(value);
}

// budgetKnobsChanged returns the PUT body containing only knobs whose
// parsed values differ from what is stored. Empty object = nothing to
// save. Errors are surfaced by the caller before this is consulted.
export function budgetKnobsChanged(
  current: PlatformBudget,
  next: { defaultMonthly: BudgetInputResult; max: BudgetInputResult; platformLimit: BudgetInputResult },
): Record<string, number | null> {
  const body: Record<string, number | null> = {};
  const add = (key: string, cur: number | null, res: BudgetInputResult) => {
    if (res.kind === "clear") {
      if (cur != null) body[key] = null;
    } else if (res.kind === "value") {
      if (cur !== res.value) body[key] = res.value;
    }
  };
  add("default_monthly_usd", current.default_monthly_usd, next.defaultMonthly);
  add("max_usd", current.max_usd, next.max);
  add("platform_monthly_limit_usd", current.platform_monthly_limit_usd, next.platformLimit);
  return body;
}

// ---------------------------------------------------------------------------
// Display helpers
// ---------------------------------------------------------------------------

// budgetLabel describes a user's budget state for tables.
export function budgetStatusLabel(status: CostBudgetStatus): string {
  if (status.monthly_budget_usd == null) return "No limit";
  return status.over_budget ? "Over budget" : "Within budget";
}

// scopeLabel maps the summary's scope to human text.
export function scopeLabel(scope: string | undefined): string {
  return scope === "all" ? "all squads (platform admin view)" : "your squads";
}
