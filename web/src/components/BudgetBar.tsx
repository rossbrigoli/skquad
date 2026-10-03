"use client";

// S-224 (S-203 WP2): horizontal "this month vs my budget" bar.
// The spend fill is clamped to the bar width; when over budget the fill
// turns the over-budget color and the overrun is called out in the sub
// line. No budget → the bar is replaced by a "no limit" note so the
// empty state never looks like a broken chart.

import { formatMoney } from "../lib/format";
import { buildBudgetModel } from "../lib/costs";

export function BudgetBar({
  spend,
  budget,
  currency = "USD",
}: {
  spend: number;
  budget: number | null | undefined;
  currency?: string;
}) {
  const model = buildBudgetModel(spend, budget);
  if (!model.hasBudget) {
    return (
      <div className="budget-bar" data-state="none">
        <div className="budget-bar-note">
          No monthly budget set — spend is unlimited (subject to platform limits).
        </div>
        <div className="budget-bar-meta">MTD spend: {formatMoney(model.spend, currency)}</div>
      </div>
    );
  }
  const pct = Math.round(model.fraction * 100);
  return (
    <div className="budget-bar" data-state={model.over ? "over" : "ok"}>
      <div className="budget-bar-track" role="img" aria-label={`${formatMoney(model.spend, currency)} of ${formatMoney(model.budget, currency)} budget used (${pct}%)`}>
        <div
          className={model.over ? "budget-bar-fill over" : "budget-bar-fill"}
          style={{ width: `${pct}%` }}
        />
      </div>
      <div className="budget-bar-meta">
        <span>
          MTD {formatMoney(model.spend, currency)} / budget {formatMoney(model.budget, currency)} ({pct}%)
        </span>
        <span className={model.over ? "budget-bar-over-label" : undefined}>
          {model.over
            ? `Over by ${formatMoney(model.spend - model.budget, currency)}`
            : `${formatMoney(model.remaining, currency)} remaining`}
        </span>
      </div>
    </div>
  );
}
