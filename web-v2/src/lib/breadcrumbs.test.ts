import { describe, expect, it } from "vitest";

import {
  agentIdFromPath,
  breadcrumbsForPath,
  taskIdFromPath,
} from "./breadcrumbs";

describe("breadcrumbsForPath", () => {
  it("renders root as a single Dashboard crumb", () => {
    expect(breadcrumbsForPath("/")).toEqual([{ label: "Dashboard", href: null }]);
  });

  it("renders single-segment routes as one plain crumb", () => {
    expect(breadcrumbsForPath("/dashboard")).toEqual([{ label: "Dashboard", href: null }]);
    expect(breadcrumbsForPath("/inbox")).toEqual([{ label: "Inbox", href: null }]);
    expect(breadcrumbsForPath("/costs")).toEqual([{ label: "Costs", href: null }]);
    expect(breadcrumbsForPath("/settings")).toEqual([{ label: "Settings", href: null }]);
  });

  it("links all crumbs except the last", () => {
    expect(breadcrumbsForPath("/squads")).toEqual([{ label: "Squads", href: null }]);
    expect(breadcrumbsForPath("/squads/s1/board")).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Squad", href: "/squads/s1" },
      { label: "Board", href: null },
    ]);
  });

  it("resolves the squad segment from the squadName hint", () => {
    expect(breadcrumbsForPath("/squads/abc123", { squadName: "Deploy Bots" })).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Deploy Bots", href: null },
    ]);
  });

  it("resolves squad and agent segments from hints", () => {
    expect(
      breadcrumbsForPath("/squads/abc/agents/ag-9", {
        squadName: "Deploy Bots",
        agentName: "Builder",
      }),
    ).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Deploy Bots", href: "/squads/abc" },
      { label: "Agents", href: "/squads/abc/agents" },
      { label: "Builder", href: null },
    ]);
  });

  it("falls back to generic labels when hints are missing", () => {
    expect(breadcrumbsForPath("/squads/abc/agents/ag-9")).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Squad", href: "/squads/abc" },
      { label: "Agents", href: "/squads/abc/agents" },
      { label: "Agent", href: null },
    ]);
    expect(breadcrumbsForPath("/squads/abc/tasks/t-1")).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Squad", href: "/squads/abc" },
      { label: "Tasks", href: "/squads/abc/tasks" },
      { label: "Task", href: null },
    ]);
  });

  it("uses the taskName hint when provided", () => {
    expect(breadcrumbsForPath("/squads/abc/tasks/t-1", { taskName: "Fix login" })).toEqual([
      { label: "Squads", href: "/squads" },
      { label: "Squad", href: "/squads/abc" },
      { label: "Tasks", href: "/squads/abc/tasks" },
      { label: "Fix login", href: null },
    ]);
  });

  it("humanises unknown static segments", () => {
    expect(breadcrumbsForPath("/settings/billing_settings")).toEqual([
      { label: "Settings", href: "/settings" },
      { label: "Billing settings", href: null },
    ]);
  });

  it("ignores trailing slashes and tolerates empty input", () => {
    expect(breadcrumbsForPath("/squads/abc/")).toEqual(breadcrumbsForPath("/squads/abc"));
    expect(breadcrumbsForPath("")).toEqual([{ label: "Dashboard", href: null }]);
  });
});

describe("agentIdFromPath", () => {
  it("extracts the agent id from agent detail routes", () => {
    expect(agentIdFromPath("/squads/s1/agents/a42")).toBe("a42");
  });

  it("returns empty string for non-agent-detail routes", () => {
    expect(agentIdFromPath("/squads/s1/agents")).toBe("");
    expect(agentIdFromPath("/squads/s1/board")).toBe("");
    expect(agentIdFromPath("/dashboard")).toBe("");
    expect(agentIdFromPath("")).toBe("");
  });
});

describe("taskIdFromPath", () => {
  it("extracts the task id from task detail routes", () => {
    expect(taskIdFromPath("/squads/s1/tasks/t7")).toBe("t7");
  });

  it("returns empty string otherwise", () => {
    expect(taskIdFromPath("/squads/s1/tasks")).toBe("");
    expect(taskIdFromPath("/squads/s1/agents/a1")).toBe("");
  });
});
