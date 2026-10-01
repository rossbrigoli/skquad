// S-190: usage chart model tests.

import { describe, expect, it } from "vitest";

import {
  buildStackedChart,
  paletteColor,
  CHART_PALETTE,
  pointValue,
  providerUsageMap,
  type UsagePoint,
  type UsageSeries,
} from "./usage";

function point(day: string, tokens: number, cost: number): UsagePoint {
  return { day, input_tokens: Math.round(tokens * 0.8), output_tokens: Math.round(tokens * 0.2), tokens, cost };
}

function series(id: string, name: string, points: UsagePoint[]): UsageSeries {
  return { id, name, points };
}

describe("paletteColor", () => {
  it("cycles deterministically", () => {
    expect(paletteColor(0)).toBe(CHART_PALETTE[0]);
    expect(paletteColor(CHART_PALETTE.length)).toBe(CHART_PALETTE[0]);
    expect(paletteColor(3)).toBe(CHART_PALETTE[3]);
  });
});

describe("pointValue", () => {
  it("selects tokens or cost by mode", () => {
    const p = point("2026-10-01", 100, 0.5);
    expect(pointValue(p, "tokens")).toBe(100);
    expect(pointValue(p, "cost")).toBe(0.5);
  });
  it("treats missing points as zero", () => {
    expect(pointValue(undefined, "tokens")).toBe(0);
    expect(pointValue(undefined, "cost")).toBe(0);
  });
});

describe("buildStackedChart", () => {
  const days = ["2026-09-30", "2026-10-01", "2026-10-02"];
  const alpha = series("a", "Alpha", [point(days[0], 10, 0.1), point(days[1], 30, 0.3)]);
  const beta = series("b", "Beta", [point(days[1], 20, 0.2), point(days[2], 5, 0.05)]);

  it("stacks segments with running offsets in token mode", () => {
    const model = buildStackedChart(days, [alpha, beta], "tokens");
    expect(model.columns).toHaveLength(3);
    expect(model.maxTotal).toBe(50);
    expect(model.hasData).toBe(true);

    const mid = model.columns[1];
    expect(mid.total).toBe(50);
    expect(mid.segments).toHaveLength(2);
    expect(mid.segments[0]).toMatchObject({ name: "Alpha", value: 30, offset: 0 });
    expect(mid.segments[1]).toMatchObject({ name: "Beta", value: 20, offset: 30 });
  });

  it("switches to dollar values in cost mode", () => {
    const model = buildStackedChart(days, [alpha, beta], "cost");
    expect(model.maxTotal).toBeCloseTo(0.5, 10);
    expect(model.columns[1].segments[0].value).toBeCloseTo(0.3, 10);
  });

  it("zero-fills days with no data and keeps legend stable", () => {
    const model = buildStackedChart(days, [alpha, beta], "tokens");
    expect(model.columns[2].segments).toHaveLength(1); // only Beta
    expect(model.legend).toEqual([
      { name: "Alpha", color: paletteColor(0) },
      { name: "Beta", color: paletteColor(1) },
    ]);
  });

  it("handles empty inputs", () => {
    const model = buildStackedChart([], [], "tokens");
    expect(model.columns).toEqual([]);
    expect(model.maxTotal).toBe(0);
    expect(model.hasData).toBe(false);
  });

  it("indexes by day string, not array position", () => {
    // Deliberately misaligned points: Beta's points array is ordered
    // [day2, day1]; day-keyed lookup must still place values correctly.
    const betaShuffled = series("b", "Beta", [point(days[2], 5, 0.05), point(days[1], 20, 0.2)]);
    const model = buildStackedChart(days, [alpha, betaShuffled], "tokens");
    expect(model.columns[1].total).toBe(50);
    expect(model.columns[2].segments[0]).toMatchObject({ name: "Beta", value: 5 });
  });

  it("falls back to series id when name is empty", () => {
    const nameless = series("zz", "", [point(days[0], 1, 0.01)]);
    const model = buildStackedChart(days, [nameless], "tokens");
    expect(model.legend[0].name).toBe("zz");
    expect(model.columns[0].segments[0].name).toBe("zz");
  });
});

describe("providerUsageMap", () => {
  it("indexes by provider id and tolerates undefined", () => {
    const map = providerUsageMap([
      { provider_id: "p1", provider_name: "One", tokens: 10, cost: 0.1, models: [] },
    ]);
    expect(map.get("p1")?.provider_name).toBe("One");
    expect(map.get("missing")).toBeUndefined();
    expect(providerUsageMap(undefined).size).toBe(0);
  });
});
