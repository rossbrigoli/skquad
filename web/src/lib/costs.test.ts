// S-224 (S-203 WP2): pure logic tests for the Cost Management helpers.
import { describe, expect, it } from "vitest";
import {
  budgetKnobsChanged,
  budgetStatusLabel,
  buildBudgetModel,
  knobToInput,
  parseBudgetInput,
  resolveCostTab,
  scopeLabel,
  seriesForSource,
  type CostSummaryPayload,
  type CostBudgetStatus,
  type PlatformBudget,
} from "./costs";
import type { UsageSeries } from "./usage";

function series(name: string, cost: number): UsageSeries {
  return {
    id: name,
    name,
    points: [{ day: "2026-10-01", input_tokens: 0, output_tokens: 0, tokens: 0, cost }],
  };
}

function payload(overrides: Partial<CostSummaryPayload> = {}): CostSummaryPayload {
  return {
    scope: "personal",
    days: ["2026-10-01"],
    mtd_start: "2026-10-01",
    total_cost: 10,
    mtd_cost: 4,
    running_cost: 4,
    by_squad: [series("Alpha", 1)],
    by_agent: [series("bot", 2)],
    by_model: [series("gpt", 3)],
    by_provider: [series("openai", 4)],
    ...overrides,
  };
}

describe("resolveCostTab", () => {
  it("gives non-admins the cost tab even if budget is requested", () => {
    expect(resolveCostTab("budget", false)).toBe("cost");
    expect(resolveCostTab("cost", false)).toBe("cost");
  });
  it("gives admins both tabs", () => {
    expect(resolveCostTab("budget", true)).toBe("budget");
    expect(resolveCostTab("cost", true)).toBe("cost");
  });
});

describe("seriesForSource", () => {
  it("maps each grouping to its payload field", () => {
    const p = payload();
    expect(seriesForSource(p, "squads")).toEqual(p.by_squad);
    expect(seriesForSource(p, "agents")).toEqual(p.by_agent);
    expect(seriesForSource(p, "models")).toEqual(p.by_model);
    expect(seriesForSource(p, "providers")).toEqual(p.by_provider);
  });
  it("tolerates a null payload and null fields", () => {
    expect(seriesForSource(null, "squads")).toEqual([]);
    expect(seriesForSource(payload({ by_model: undefined as unknown as UsageSeries[] }), "models")).toEqual([]);
  });
});

describe("buildBudgetModel", () => {
  it("computes fraction and remaining under budget", () => {
    const m = buildBudgetModel(25, 100);
    expect(m.hasBudget).toBe(true);
    expect(m.fraction).toBeCloseTo(0.25);
    expect(m.over).toBe(false);
    expect(m.remaining).toBeCloseTo(75);
  });
  it("clamps fraction at 1 and flags over-budget", () => {
    const m = buildBudgetModel(150, 100);
    expect(m.fraction).toBe(1);
    expect(m.over).toBe(true);
    expect(m.remaining).toBe(0);
  });
  it("treats null/zero/negative budgets as no limit", () => {
    for (const b of [null, undefined, 0, -5]) {
      const m = buildBudgetModel(10, b);
      expect(m.hasBudget).toBe(false);
      expect(m.over).toBe(false);
    }
  });
  it("treats exactly-at-budget as not over", () => {
    const m = buildBudgetModel(100, 100);
    expect(m.over).toBe(false);
    expect(m.fraction).toBe(1);
  });
});

describe("parseBudgetInput", () => {
  it("empty string clears the knob", () => {
    expect(parseBudgetInput("")).toEqual({ kind: "clear" });
    expect(parseBudgetInput("   ")).toEqual({ kind: "clear" });
  });
  it("parses valid numbers", () => {
    expect(parseBudgetInput("42")).toEqual({ kind: "value", value: 42 });
    expect(parseBudgetInput("12.5")).toEqual({ kind: "value", value: 12.5 });
    expect(parseBudgetInput("0")).toEqual({ kind: "value", value: 0 });
  });
  it("rejects negatives, garbage and over-max", () => {
    expect(parseBudgetInput("-1").kind).toBe("error");
    expect(parseBudgetInput("abc").kind).toBe("error");
    expect(parseBudgetInput("2000000000").kind).toBe("error");
  });
});

describe("knobToInput", () => {
  it("null/undefined render as empty, numbers as strings", () => {
    expect(knobToInput(null)).toBe("");
    expect(knobToInput(undefined)).toBe("");
    expect(knobToInput(99.5)).toBe("99.5");
  });
});

describe("budgetKnobsChanged", () => {
  const current: PlatformBudget = {
    default_monthly_usd: 100,
    max_usd: 500,
    platform_monthly_limit_usd: null,
    platform_mtd_cost: 42,
  };
  const ok = (v: string | null) => (v === null ? { kind: "clear" as const } : parseBudgetInput(v));

  it("returns empty when nothing changed", () => {
    expect(
      budgetKnobsChanged(current, {
        defaultMonthly: ok("100"),
        max: ok("500"),
        platformLimit: ok(""),
      }),
    ).toEqual({});
  });
  it("includes only changed knobs, null for clears", () => {
    expect(
      budgetKnobsChanged(current, {
        defaultMonthly: ok("250"),
        max: ok("500"),
        platformLimit: ok("9000"),
      }),
    ).toEqual({ default_monthly_usd: 250, platform_monthly_limit_usd: 9000 });
  });
  it("clearing a stored knob sends explicit null", () => {
    expect(
      budgetKnobsChanged(current, {
        defaultMonthly: ok("100"),
        max: ok(null),
        platformLimit: ok(""),
      }),
    ).toEqual({ max_usd: null });
  });
});

describe("display helpers", () => {
  const base: CostBudgetStatus = {
    user_id: "u1",
    monthly_budget_usd: 100,
    mtd_cost: 50,
    remaining_usd: 50,
    over_budget: false,
  };
  it("labels budget states", () => {
    expect(budgetStatusLabel(base)).toBe("Within budget");
    expect(budgetStatusLabel({ ...base, over_budget: true })).toBe("Over budget");
    expect(budgetStatusLabel({ ...base, monthly_budget_usd: null, remaining_usd: null })).toBe("No limit");
  });
  it("maps scope", () => {
    expect(scopeLabel("all")).toContain("platform admin");
    expect(scopeLabel("personal")).toBe("your squads");
    expect(scopeLabel(undefined)).toBe("your squads");
  });
});
