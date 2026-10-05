// S-230: AI provider tiles — ONLINE indicator (green when online),
// rolling 30-day aggregated cost, per-model rows, offline/inactive
// degradation. Node-env static render, same pattern as AgentTiles.test.tsx.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { ProviderTile, ProviderTilesGrid, onlineIndicatorLabel, providerTileClass } from "./ProviderTiles";
import type { DashboardProvider } from "../lib/dashboard";
import type { ProviderUsage } from "../lib/usage";

const online: DashboardProvider = { id: "p1", name: "OpenAI", kind: "openai", status: "active", online: true, latency_ms: 42 };
const offline: DashboardProvider = { id: "p2", name: "Local", kind: "ollama", status: "active", online: false, error: "timeout" };
const deprecated: DashboardProvider = { id: "p3", name: "Old", kind: "openai", status: "deprecated", online: true };

const usage: ProviderUsage = {
  provider_id: "p1",
  provider_name: "OpenAI",
  cost: 25.36,
  tokens: 12000,
  models: [
    { model: "gpt-4.1-mini", cost: 0.29, tokens: 9000 },
    { model: "gpt-6-astra", cost: 25.07, tokens: 3000 },
  ],
};

describe("providerTileClass (S-230)", () => {
  it("online tiles carry the online modifier", () => {
    expect(providerTileClass(true)).toBe("provider-tile provider-tile--online");
  });
  it("offline tiles carry no modifier", () => {
    expect(providerTileClass(false)).toBe("provider-tile");
  });
  it("online indicator label is uppercase ONLINE/OFFLINE", () => {
    expect(onlineIndicatorLabel(true)).toBe("ONLINE");
    expect(onlineIndicatorLabel(false)).toBe("OFFLINE");
  });
});

describe("ProviderTile (S-230)", () => {
  it("online provider tile shows the green ONLINE badge and 30-day cost", () => {
    const html = renderToStaticMarkup(createElement(ProviderTile, { provider: online, usageRow: usage, currency: "USD" }));
    expect(html).toContain("provider-tile--online");
    expect(html).toContain("ONLINE");
    expect(html).toContain("provider-online-dot");
    expect(html).toContain("last 30 days USD 25.3600 · 12K tokens");
    expect(html).toContain("gpt-6-astra");
    expect(html).toContain("gpt-4.1-mini");
  });

  it("offline provider tile shows OFFLINE and the error, never the online modifier", () => {
    const html = renderToStaticMarkup(createElement(ProviderTile, { provider: offline, usageRow: undefined, currency: "USD" }));
    expect(html).not.toContain("provider-tile--online");
    expect(html).toContain("OFFLINE");
    expect(html).toContain("timeout");
    expect(html).toContain("no usage in the last 30 days");
  });

  it("deprecated provider is never shown as online (lifecycle beats liveness)", () => {
    const html = renderToStaticMarkup(createElement(ProviderTile, { provider: deprecated, usageRow: undefined, currency: "USD" }));
    expect(html).not.toContain("provider-tile--online");
    expect(html).toContain("OFFLINE");
  });
});

describe("ProviderTilesGrid (S-230)", () => {
  it("renders one tile per provider wired to the usage map", () => {
    const map = new Map([[ "p1", usage ]]);
    const html = renderToStaticMarkup(
      createElement(ProviderTilesGrid, { providers: [online, offline], usageMap: map, currency: "USD" }),
    );
    expect(html).toContain('class="agent-tile-grid"');
    expect((html.match(/class="provider-tile[ "]/g) ?? []).length).toBe(2);
    expect(html).toContain("provider-tile--online");
    expect(html).toContain("no usage in the last 30 days");
  });
});
