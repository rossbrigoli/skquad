// TG-9 slice C: per-resource rollup math.
import { describe, expect, it } from "vitest";
import type { AuditEntry } from "./api";
import { aggregateResourceRollups, rollupTotals } from "./resourceMetering";

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: `e${Math.random().toString(36).slice(2)}`,
    actor_type: "agent",
    actor_id: "ag1",
    action: "task.update",
    resource_type: "task",
    resource_id: "t1",
    timestamp: "2026-10-05T00:00:00Z",
    ...overrides,
  };
}

describe("aggregateResourceRollups", () => {
  it("buckets by (type,id) and counts decisions", () => {
    const rows = aggregateResourceRollups([
      entry({ resource_type: "tool_resource", resource_id: "r1" }),
      entry({ resource_type: "tool_resource", resource_id: "r1", metadata: { gate: "denied", reason: "denied_replayed" } }),
      entry({ resource_type: "tool_resource", resource_id: "r1", metadata: { gate: "pending" } }),
      entry({ resource_type: "task", resource_id: "r1" }),
    ]);
    expect(rows).toHaveLength(2);
    const tool = rows.find((r) => r.key === "tool_resource:r1");
    expect(tool).toMatchObject({ calls: 3, allowed: 1, denied: 1, pending: 1 });
    // Same id, different type → separate bucket.
    expect(rows.find((r) => r.key === "task:r1")).toMatchObject({ calls: 1, allowed: 1 });
  });

  it("skips entries without a resource reference", () => {
    const rows = aggregateResourceRollups([
      entry({ resource_type: "", resource_id: "x" }),
      entry({ resource_type: "task", resource_id: "  " }),
    ]);
    expect(rows).toHaveLength(0);
  });

  it("tracks the newest timestamp as lastSeen regardless of input order", () => {
    const rows = aggregateResourceRollups([
      entry({ timestamp: "2026-10-05T00:00:00Z" }),
      entry({ timestamp: "2026-10-09T00:00:00Z" }),
      entry({ timestamp: "2026-10-01T00:00:00Z" }),
    ]);
    expect(rows[0].lastSeen).toBe("2026-10-09T00:00:00Z");
  });

  it("leaves lastSeen empty when no entry carries a timestamp", () => {
    const rows = aggregateResourceRollups([entry({ timestamp: undefined })]);
    expect(rows[0].lastSeen).toBe("");
  });

  it("sorts by call volume desc, then key asc", () => {
    const rows = aggregateResourceRollups([
      entry({ resource_type: "b", resource_id: "1" }),
      entry({ resource_type: "a", resource_id: "2" }),
      entry({ resource_type: "a", resource_id: "2" }),
      entry({ resource_type: "c", resource_id: "3" }),
      entry({ resource_type: "c", resource_id: "3" }),
    ]);
    expect(rows.map((r) => r.key)).toEqual(["a:2", "c:3", "b:1"]);
  });

  it("empty input yields no rows", () => {
    expect(aggregateResourceRollups([])).toEqual([]);
  });
});

describe("rollupTotals", () => {
  it("sums across rows", () => {
    const rows = aggregateResourceRollups([
      entry({ resource_type: "x", resource_id: "1" }),
      entry({ resource_type: "x", resource_id: "1", metadata: { gate: "denied" } }),
      entry({ resource_type: "y", resource_id: "2", metadata: { gate: "pending" } }),
    ]);
    expect(rollupTotals(rows)).toEqual({
      resources: 2,
      calls: 3,
      allowed: 1,
      denied: 1,
      pending: 1,
    });
  });
  it("zero state for no rows", () => {
    expect(rollupTotals([])).toEqual({ resources: 0, calls: 0, allowed: 0, denied: 0, pending: 0 });
  });
});
