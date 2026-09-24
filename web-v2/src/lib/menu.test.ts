import { describe, expect, it } from "vitest";
import type { Agent, Squad } from "./api";
import type { DashboardPayload } from "./dashboard";
import {
  agentHref,
  agentsSectionActive,
  buildGlobalAgentGroups,
  buildSquadAgentSubitems,
  buildSquadSubitems,
  effectiveExpanded,
  isSubitemActive,
  squadIdFromPath,
} from "./menu";

const squad = (id: string, name: string): Squad => ({ id, name });
const agent = (id: string, squadId: string, name: string): Agent => ({ id, squad_id: squadId, name });

describe("squadIdFromPath", () => {
  it("extracts the squad id inside a squad route", () => {
    expect(squadIdFromPath("/squads/abc-123")).toBe("abc-123");
    expect(squadIdFromPath("/squads/abc-123/board")).toBe("abc-123");
    expect(squadIdFromPath("/squads/abc-123/agents/ag-9")).toBe("abc-123");
    expect(squadIdFromPath("/squads/abc-123/tasks/t1")).toBe("abc-123");
  });

  it("returns empty for the squads index and non-squad routes", () => {
    expect(squadIdFromPath("/squads")).toBe("");
    expect(squadIdFromPath("/dashboard")).toBe("");
    expect(squadIdFromPath("/inbox")).toBe("");
    expect(squadIdFromPath("")).toBe("");
  });
});

describe("agentsSectionActive", () => {
  it("is true on the squad agents tab and agent detail pages", () => {
    expect(agentsSectionActive("/squads/s1/agents")).toBe(true);
    expect(agentsSectionActive("/squads/s1/agents/a1")).toBe(true);
  });

  it("is false elsewhere", () => {
    expect(agentsSectionActive("/squads/s1")).toBe(false);
    expect(agentsSectionActive("/squads/s1/board")).toBe(false);
    expect(agentsSectionActive("/squads")).toBe(false);
    expect(agentsSectionActive("/dashboard")).toBe(false);
  });
});

describe("buildSquadSubitems", () => {
  it("maps squads to alphabetical name links to the squad overview", () => {
    const items = buildSquadSubitems([squad("s2", "Suicide Squad"), squad("s1", "Alpha Squad")]);
    expect(items).toEqual([
      { href: "/squads/s1", label: "Alpha Squad" },
      { href: "/squads/s2", label: "Suicide Squad" },
    ]);
  });

  it("handles null/empty", () => {
    expect(buildSquadSubitems(null)).toEqual([]);
    expect(buildSquadSubitems([])).toEqual([]);
  });
});

describe("buildSquadAgentSubitems", () => {
  it("maps a squad's agents to alphabetical links to the agent pages", () => {
    const items = buildSquadAgentSubitems([
      agent("a2", "s1", "Zeta"),
      agent("a1", "s1", "Enzo"),
    ]);
    expect(items).toEqual([
      { href: "/squads/s1/agents/a1", label: "Enzo" },
      { href: "/squads/s1/agents/a2", label: "Zeta" },
    ]);
  });

  it("handles null/empty", () => {
    expect(buildSquadAgentSubitems(null)).toEqual([]);
  });
});

describe("buildGlobalAgentGroups", () => {
  const payload = {
    scope: "all",
    squads: [
      {
        id: "s2",
        name: "Suicide Squad",
        agents: [{ id: "a3", squad_id: "s2", name: "Mary", status: "idle" }],
      },
      {
        id: "s1",
        name: "Skquad Engineering",
        agents: [
          { id: "a1", squad_id: "s1", name: "Enzo", status: "idle" },
          { id: "a2", squad_id: "s1", name: "Alice", status: "busy" },
        ],
      },
      { id: "s3", name: "Empty Squad", agents: [] },
    ],
    providers: [],
    resources: [],
  } as DashboardPayload;

  it("groups all accessible agents by squad, squads alphabetical, agents alphabetical", () => {
    expect(buildGlobalAgentGroups(payload)).toEqual([
      {
        squadId: "s1",
        squadName: "Skquad Engineering",
        items: [
          { href: "/squads/s1/agents/a2", label: "Alice" },
          { href: "/squads/s1/agents/a1", label: "Enzo" },
        ],
      },
      {
        squadId: "s2",
        squadName: "Suicide Squad",
        items: [{ href: "/squads/s2/agents/a3", label: "Mary" }],
      },
    ]);
  });

  it("handles null/empty payloads", () => {
    expect(buildGlobalAgentGroups(null)).toEqual([]);
    expect(buildGlobalAgentGroups({ scope: "personal", squads: [], providers: [], resources: [] })).toEqual([]);
  });
});

describe("isSubitemActive", () => {
  it("matches exact and nested routes", () => {
    expect(isSubitemActive("/squads/s1", "/squads/s1")).toBe(true);
    expect(isSubitemActive("/squads/s1/agents/a1", "/squads/s1/agents/a1")).toBe(true);
    expect(isSubitemActive("/squads/s1/board", "/squads/s1")).toBe(true);
  });

  it("does not match sibling squads sharing an id prefix", () => {
    expect(isSubitemActive("/squads/s12/board", "/squads/s1")).toBe(false);
    expect(isSubitemActive("/dashboard", "/squads/s1")).toBe(false);
  });
});

describe("effectiveExpanded", () => {
  it("user toggle wins over route state", () => {
    expect(effectiveExpanded(true, false)).toBe(true);
    expect(effectiveExpanded(false, true)).toBe(false);
  });

  it("falls back to route-driven auto-expand", () => {
    expect(effectiveExpanded(undefined, true)).toBe(true);
    expect(effectiveExpanded(undefined, false)).toBe(false);
  });
});

describe("agentHref", () => {
  it("builds the agent page route", () => {
    expect(agentHref("s1", "a1")).toBe("/squads/s1/agents/a1");
  });
});
