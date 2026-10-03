// S-211 items 5–7: agent tiles — robot icon, name, role, status;
// running → pulsing halo class, failed/error → red outline class.
// Node-env static render, same pattern as ToolTiles.test.tsx.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { AgentTilesGrid, agentTileClass } from "./AgentTiles";
import type { Agent } from "../lib/api";

const agents: Agent[] = [
  { id: "a1", squad_id: "s1", name: "Ross", role: "architect", status: "busy" },
  { id: "a2", squad_id: "s1", name: "Breaker", role: "chaos", status: "failed" },
  { id: "a3", squad_id: "s1", name: "Sleeper", role: "", status: "idle" },
  { id: "a4", squad_id: "s1", name: "Errored", role: "qa", status: "error" },
  { id: "a5", squad_id: "s1", name: "Paused", role: "ops", status: "paused" },
];

describe("agentTileClass (S-211)", () => {
  it("running agents get the halo modifier", () => {
    expect(agentTileClass("running")).toBe("agent-tile agent-tile--running");
  });

  it("failed/error agents get the red-outline modifier", () => {
    expect(agentTileClass("error")).toBe("agent-tile agent-tile--failed");
  });

  it("idle/paused tiles carry no state modifier", () => {
    expect(agentTileClass("idle")).toBe("agent-tile");
    expect(agentTileClass("paused")).toBe("agent-tile");
  });
});

describe("AgentTilesGrid (S-211)", () => {
  const html = renderToStaticMarkup(createElement(AgentTilesGrid, { agents }));

// tileClassFor extracts the class attribute of the anchor linking to an
// agent's detail page (class precedes href in the rendered markup).
function tileClassFor(markup: string, agentId: string): string {
  const m = markup.match(new RegExp(`<a class="([^"]+)"[^>]*href="/squads/s1/agents/${agentId}"`));
  return m ? m[1] : "";
}

  it("renders one tile per agent linking to the agent detail page", () => {
    expect(html).toContain('class="agent-tile-grid"');
    expect((html.match(/class="agent-tile[ "]/g) ?? []).length).toBe(5);
    expect(html).toContain('href="/squads/s1/agents/a1"');
    expect(html).toContain('href="/squads/s1/agents/a5"');
  });

  it("busy agent renders with the running halo class", () => {
    expect(tileClassFor(html, "a1")).toContain("agent-tile--running");
  });

  it("failed and error agents render with the failed class", () => {
    expect(tileClassFor(html, "a2")).toContain("agent-tile--failed");
    expect(tileClassFor(html, "a4")).toContain("agent-tile--failed");
  });

  it("idle agent tile has no halo/failed modifier", () => {
    const cls = tileClassFor(html, "a3");
    expect(cls).toBe("agent-tile");
  });

  it("each tile shows the robot icon, name, role and status chip", () => {
    const tile = html.split('href="/squads/s1/agents/a1"')[1].split("</a>")[0];
    expect(tile).toContain("<svg"); // robot icon
    expect(tile).toContain("Ross");
    expect(tile).toContain("architect");
    expect(tile).toContain('class="chip chip-running"');
    const roleless = html.split('href="/squads/s1/agents/a3"')[1];
    expect(roleless).toContain("no role set");
  });
});
