// S-224: BudgetBar static-markup render tests (repo node-env philosophy).
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { BudgetBar } from "./BudgetBar";

describe("BudgetBar", () => {
  it("renders a filled bar with percent + remaining under budget", () => {
    const html = renderToStaticMarkup(createElement(BudgetBar, { spend: 25, budget: 100 }));
    expect(html).toContain("budget-bar-track");
    expect(html).toContain('style="width:25%"');
    expect(html).toContain("25%");
    expect(html).toContain("remaining");
    expect(html).not.toContain("Over by");
  });

  it("clamps the bar at 100% and calls out the overrun", () => {
    const html = renderToStaticMarkup(createElement(BudgetBar, { spend: 150, budget: 100 }));
    expect(html).toContain('style="width:100%"');
    expect(html).toContain("Over by");
    expect(html).toContain('data-state="over"');
  });

  it("shows the no-limit note when budget is null", () => {
    const html = renderToStaticMarkup(createElement(BudgetBar, { spend: 10, budget: null }));
    expect(html).toContain('data-state="none"');
    expect(html).toContain("No monthly budget set");
    expect(html).not.toContain("budget-bar-track");
  });
});
