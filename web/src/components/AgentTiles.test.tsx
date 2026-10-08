// S-211 items 5–7: agent tiles — robot icon, name, role, status;
// running → pulsing halo class, failed/error → red outline class.
// Node-env static render, same pattern as ToolTiles.test.tsx.
import { describe, expect, it } from "vitest";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { AgentTile, AgentTilesGrid, agentTaskLine, agentTileClass } from "./AgentTiles";
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

  it("S-242: variant appends a contextual modifier class", () => {
    expect(agentTileClass("idle", "dashboard")).toBe("agent-tile agent-tile--dashboard");
    expect(agentTileClass("running", "dashboard")).toBe("agent-tile agent-tile--dashboard agent-tile--running");
  });
});

describe("agentTaskLine (S-242)", () => {
  it("prefers current_task over last_task", () => {
    const line = agentTaskLine({
      current_task: { id: "t2", ref: "S-102", title: "Fix the login bug" },
      last_task: { id: "t1", ref: "S-101", title: "Old work" },
    });
    expect(line).toBe("S-102 · Fix the login bug");
  });

  it("falls back to last_task when nothing is running", () => {
    const line = agentTaskLine({ last_task: { id: "t1", ref: "S-7", title: "Ship tiles" } });
    expect(line).toBe("S-7 · Ship tiles");
  });

  it("falls back to a short id prefix when no ref is set", () => {
    const line = agentTaskLine({ last_task: { id: "abcdef123456", title: "No ref here" } });
    expect(line).toBe("abcdef12 · No ref here");
  });

  it("renders just the ref when the title is blank", () => {
    expect(agentTaskLine({ current_task: { id: "t9", ref: "S-9" } })).toBe("S-9");
  });

  it("returns null when neither task exists", () => {
    expect(agentTaskLine({})).toBeNull();
    expect(agentTaskLine({ current_task: null, last_task: null })).toBeNull();
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

  it("S-230/S-242: costLabel renders a plain-amount cost line; omitted means no cost element", () => {
    const withCost = renderToStaticMarkup(
      createElement(AgentTile, { agent: agents[0], href: "/squads/s1/agents/a1", costLabel: "USD 1.2500" }),
    );
    expect(withCost).toContain('class="agent-tile-cost mono"');
    expect(withCost).toContain("USD 1.2500");
    // S-242 req 2: the tile never says "last 30 days".
    expect(withCost).not.toContain("last 30 days");
    const withoutCost = renderToStaticMarkup(createElement(AgentTile, { agent: agents[0], href: "/squads/s1/agents/a1" }));
    expect(withoutCost).not.toContain("agent-tile-cost");
  });

  it("S-242 req 3: model renders as a muted truncated line, hidden when absent", () => {
    const withModel = renderToStaticMarkup(
      createElement(AgentTile, { agent: { ...agents[0], model: "claude-sonnet-4.5" }, href: "/squads/s1/agents/a1" }),
    );
    expect(withModel).toContain('class="agent-tile-model"');
    expect(withModel).toContain("claude-sonnet-4.5");
    const withoutModel = renderToStaticMarkup(createElement(AgentTile, { agent: agents[0], href: "/squads/s1/agents/a1" }));
    expect(withoutModel).not.toContain("agent-tile-model");
  });

  it("S-242 req 4: task line carries the truncation classes and prefers current over last", () => {
    const htmlTile = renderToStaticMarkup(
      createElement(AgentTile, {
        agent: {
          ...agents[0],
          current_task: { id: "t2", ref: "S-102", title: "Fix the login bug" },
          last_task: { id: "t1", ref: "S-101", title: "Old work" },
        },
        href: "/squads/s1/agents/a1",
      }),
    );
    expect(htmlTile).toContain('class="agent-tile-task mono"');
    expect(htmlTile).toContain("S-102 · Fix the login bug");
    expect(htmlTile).not.toContain("Old work");
    const noTasks = renderToStaticMarkup(createElement(AgentTile, { agent: agents[0], href: "/squads/s1/agents/a1" }));
    expect(noTasks).not.toContain("agent-tile-task");
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
