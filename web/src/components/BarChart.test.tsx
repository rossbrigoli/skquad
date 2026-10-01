// S-190: BarChart renders the ChartModel as stacked SVG bars with a
// legend; empty models render the empty-state message. Static-markup
// render per the repo's node-environment test philosophy.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { BarChart } from "./BarChart";
import { buildStackedChart, type UsageSeries } from "../lib/usage";

const days = ["2026-09-30", "2026-10-01", "2026-10-02"];

function series(id: string, name: string, values: Record<string, number>): UsageSeries {
  return {
    id,
    name,
    points: days.map((day) => ({
      day,
      input_tokens: values[day] ?? 0,
      output_tokens: 0,
      tokens: values[day] ?? 0,
      cost: (values[day] ?? 0) / 1000,
    })),
  };
}

const identity = (n: number) => String(n);

describe("BarChart", () => {
  it("renders one svg with stacked rects and legend entries", () => {
    const model = buildStackedChart(days, [
      series("a", "Alpha", { "2026-10-01": 30 }),
      series("b", "Beta", { "2026-10-01": 20, "2026-10-02": 5 }),
    ], "tokens");
    const html = renderToStaticMarkup(createElement(BarChart, { model, formatValue: identity }));
    expect(html).toContain("<svg");
    // 3 segments total: Alpha×1, Beta×2.
    expect((html.match(/<rect/g) ?? []).length).toBe(3);
    expect(html).toContain("Alpha");
    expect(html).toContain("Beta");
    expect(html).toContain("chart-legend");
  });

  it("renders the empty state when there is no data", () => {
    const model = buildStackedChart(days, [series("a", "Alpha", {})], "tokens");
    const html = renderToStaticMarkup(createElement(BarChart, { model, formatValue: identity }));
    expect(html).toContain("No usage recorded");
    expect(html).not.toContain("<svg");
  });

  it("includes DD-MMM day labels (S-201) and tooltip titles", () => {
    const model = buildStackedChart(days, [series("a", "Alpha", { "2026-10-01": 30 })], "tokens");
    const html = renderToStaticMarkup(createElement(BarChart, { model, formatValue: identity }));
    // Axis labels are HTML now (fixed page-text size), formatted DD-MMM.
    expect(html).toContain("chart-xlabel");
    expect(html).toContain("01-Oct");
    expect(html).toContain("2026-10-01 · Alpha: 30");
  });
});
