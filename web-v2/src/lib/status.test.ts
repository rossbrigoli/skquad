import { describe, expect, it } from "vitest";
import { agentStatus, statusAttention, taskStatus } from "./status";
import { formatCost, formatRelativeTime, leaseState } from "./format";
import type { Agent, MeteringSummary, Task } from "./api";

function task(overrides: Partial<Task> = {}): Task {
  return {
    id: "t1",
    board_id: "b1",
    squad_id: "s1",
    title: "test task",
    status: "todo",
    ...overrides,
  };
}

function futureIso(ms = 60_000): string {
  return new Date(Date.now() + ms).toISOString();
}

describe("taskStatus", () => {
  it("maps plain todo tasks", () => {
    expect(taskStatus(task())).toBe("todo");
  });

  it("maps blocked and in-review regardless of lease", () => {
    expect(taskStatus(task({ status: "blocked" }))).toBe("blocked");
    expect(taskStatus(task({ status: "in-review", execution_id: "e1", lease_expires_at: futureIso() }))).toBe("in-review");
  });

  it("maps done", () => {
    expect(taskStatus(task({ status: "done" }))).toBe("done");
  });

  it("detects running from a live lease", () => {
    expect(taskStatus(task({ status: "in-progress", execution_id: "e1", lease_expires_at: futureIso() }))).toBe("running");
  });

  it("detects stalled from an expired lease", () => {
    expect(
      taskStatus(task({ status: "in-progress", execution_id: "e1", lease_expires_at: new Date(Date.now() - 1000).toISOString() })),
    ).toBe("stalled");
  });

  it("treats Go zero-time leases as idle, not stalled", () => {
    expect(taskStatus(task({ status: "in-progress", execution_id: "e1", lease_expires_at: "0001-01-01T00:00:00Z" }))).toBe("todo");
  });
});

describe("agentStatus", () => {
  const agent = (status?: string): Agent =>
    ({ id: "a1", squad_id: "s1", name: "a", status } as Agent);

  it("maps error/failed to error", () => {
    expect(agentStatus(agent("error"))).toBe("error");
    expect(agentStatus(agent("failed"))).toBe("error");
  });

  it("maps busy to running and idle otherwise", () => {
    expect(agentStatus(agent("busy"))).toBe("running");
    expect(agentStatus(agent("idle"))).toBe("idle");
    expect(agentStatus(agent(undefined))).toBe("idle");
  });

  it("maps paused", () => {
    expect(agentStatus(agent("paused"))).toBe("paused");
  });
});

describe("attention ordering", () => {
  it("orders error before stalled before blocked before running", () => {
    expect(statusAttention.error).toBeLessThan(statusAttention.stalled);
    expect(statusAttention.stalled).toBeLessThan(statusAttention.blocked);
    expect(statusAttention.blocked).toBeLessThan(statusAttention.running);
  });
});

describe("format helpers", () => {
  it("formats cost with currency", () => {
    const summary: MeteringSummary = { cost: 1.23456, currency: "USD", input_tokens: 10, output_tokens: 20 };
    expect(formatCost(summary)).toBe("USD 1.2346");
    expect(formatCost(null)).toBe("-");
  });

  it("relative time handles missing/invalid", () => {
    expect(formatRelativeTime(undefined)).toBe("");
    expect(formatRelativeTime("nonsense")).toBe("");
  });

  it("leaseState idle on missing fields", () => {
    expect(leaseState(task())).toBe("idle");
  });
});
