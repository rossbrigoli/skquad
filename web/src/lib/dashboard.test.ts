import { describe, expect, it } from "vitest";
import {
  dashboardTotals,
  providerChip,
  resourceChip,
  taskCount,
  type DashboardPayload,
  type DashboardProvider,
} from "./dashboard";

function payload(overrides: Partial<DashboardPayload> = {}): DashboardPayload {
  return { scope: "personal", squads: [], providers: [], resources: [], ...overrides };
}

describe("taskCount", () => {
  it("reads counts by status and defaults to 0", () => {
    const squad = { id: "s1", name: "A", task_counts: { todo: 3, "in-progress": 2 } };
    expect(taskCount(squad, "todo")).toBe(3);
    expect(taskCount(squad, "in-progress")).toBe(2);
    expect(taskCount(squad, "done")).toBe(0);
    expect(taskCount({ id: "s2", name: "B" }, "todo")).toBe(0);
  });
});

describe("providerChip", () => {
  const base: DashboardProvider = { id: "p1", name: "P", status: "active", online: true };

  it("shows online for reachable active providers", () => {
    expect(providerChip(base)).toEqual({ label: "online", className: "chip chip-running" });
  });

  it("shows offline for unreachable active providers", () => {
    expect(providerChip({ ...base, online: false })).toEqual({ label: "offline", className: "chip chip-error" });
  });

  it("shows inactive (not offline) for deprecated providers", () => {
    expect(providerChip({ ...base, status: "deprecated", online: false })).toEqual({
      label: "inactive",
      className: "chip chip-paused",
    });
  });
});

describe("resourceChip", () => {
  it("maps active/deprecated/unknown", () => {
    expect(resourceChip("active")).toEqual({ label: "active", className: "chip chip-done" });
    expect(resourceChip("deprecated")).toEqual({ label: "deprecated", className: "chip chip-paused" });
    expect(resourceChip("")).toEqual({ label: "unknown", className: "chip chip-idle" });
  });
});

describe("dashboardTotals", () => {
  it("handles null payload", () => {
    expect(dashboardTotals(null)).toEqual({
      squads: 0,
      agents: 0,
      agentsRunning: 0,
      agentsIdle: 0,
      agentsError: 0,
      totalCost: 0,
      currency: "USD",
    });
  });

  it("aggregates squads, agent states and cost", () => {
    const totals = dashboardTotals(
      payload({
        squads: [
          {
            id: "s1",
            name: "Alpha",
            cost: { cost: 0.01, currency: "USD", input_tokens: 10, output_tokens: 2 },
            agents: [
              { id: "a1", squad_id: "s1", name: "busy bot", status: "busy" },
              { id: "a2", squad_id: "s1", name: "broken bot", status: "error" },
              { id: "a3", squad_id: "s1", name: "sleepy bot", status: "idle" },
            ],
          },
          {
            id: "s2",
            name: "Beta",
            cost: { cost: 0.002, currency: "USD" },
            agents: [{ id: "a4", squad_id: "s2", name: "paused bot", status: "paused" }],
          },
        ],
      }),
    );
    expect(totals.squads).toBe(2);
    expect(totals.agents).toBe(4);
    expect(totals.agentsRunning).toBe(1);
    expect(totals.agentsError).toBe(1);
    expect(totals.agentsIdle).toBe(2); // idle + paused both land in the non-running bucket
    expect(totals.totalCost).toBeCloseTo(0.012, 10);
    expect(totals.currency).toBe("USD");
  });
});
