// S-269: per-resource metering from the real CP endpoint.
import { describe, expect, it } from "vitest";
import {
  listResourceMeteringPath,
  parseResourceMeteringRow,
  parseResourceMeteringRows,
  resourceMeteringPath,
  resourceMeteringTotals,
  type ResourceMeteringRow,
} from "./resourceMetering";

function row(overrides: Partial<ResourceMeteringRow> = {}): ResourceMeteringRow {
  return {
    resourceId: "r1",
    resourceType: "rest",
    name: "paid-api",
    metered: true,
    calls: 2,
    cost: 0.1,
    currency: "USD",
    lastSeen: "2026-10-17T00:00:00Z",
    ...overrides,
  };
}

describe("resourceMeteringPath", () => {
  it("builds the single-resource path with optional since", () => {
    expect(resourceMeteringPath("abc")).toBe("/resources/abc/metering");
    expect(resourceMeteringPath("abc", "2026-10-01T00:00:00Z")).toBe(
      "/resources/abc/metering?since=2026-10-01T00%3A00%3A00Z",
    );
    expect(resourceMeteringPath("a/b")).toBe("/resources/a%2Fb/metering");
  });

  it("builds the list path with optional filters", () => {
    expect(listResourceMeteringPath()).toBe("/metering/resources");
    expect(listResourceMeteringPath("2026-10-01T00:00:00Z", "rest")).toBe(
      "/metering/resources?since=2026-10-01T00%3A00%3A00Z&type=rest",
    );
    expect(listResourceMeteringPath(undefined, "mcp")).toBe("/metering/resources?type=mcp");
  });
});

describe("parseResourceMeteringRow", () => {
  it("normalises the CP snake_case wire shape", () => {
    const parsed = parseResourceMeteringRow({
      resource_id: "r9",
      resource_type: "mcp",
      name: "toolsrv",
      metered: true,
      calls: 7,
      cost: 0.42,
      currency: "USD",
      last_seen: "2026-10-17T09:00:00Z",
    });
    expect(parsed).toEqual({
      resourceId: "r9",
      resourceType: "mcp",
      name: "toolsrv",
      metered: true,
      calls: 7,
      cost: 0.42,
      currency: "USD",
      lastSeen: "2026-10-17T09:00:00Z",
    });
  });

  it("treats non-metered as an explicit zero-cost row", () => {
    const parsed = parseResourceMeteringRow({
      resource_id: "free1",
      resource_type: "skill",
      name: "free-skill",
      metered: false,
      calls: 5,
      cost: 0,
    });
    expect(parsed?.metered).toBe(false);
    expect(parsed?.calls).toBe(5);
    expect(parsed?.cost).toBe(0);
    expect(parsed?.currency).toBe("USD"); // default when wire omits it
  });

  it("drops rows without a resource id and degrades bad numbers to 0", () => {
    expect(parseResourceMeteringRow({ name: "orphan" })).toBeNull();
    expect(parseResourceMeteringRow(null)).toBeNull();
    const parsed = parseResourceMeteringRow({ resource_id: "x", calls: "nope", cost: null });
    expect(parsed?.calls).toBe(0);
    expect(parsed?.cost).toBe(0);
  });
});

describe("parseResourceMeteringRows", () => {
  it("accepts the {rows:[...]} envelope and a bare array", () => {
    const envelope = { rows: [{ resource_id: "b", calls: 1 }, { resource_id: "a", calls: 3 }] };
    expect(parseResourceMeteringRows(envelope).map((r) => r.resourceId)).toEqual(["a", "b"]);
    expect(parseResourceMeteringRows([{ resource_id: "c" }]).map((r) => r.resourceId)).toEqual(["c"]);
  });

  it("returns empty for garbage payloads", () => {
    expect(parseResourceMeteringRows(undefined)).toEqual([]);
    expect(parseResourceMeteringRows("nope")).toEqual([]);
    expect(parseResourceMeteringRows({ nope: true })).toEqual([]);
  });
});

describe("resourceMeteringTotals", () => {
  it("sums resources, calls and cost", () => {
    const totals = resourceMeteringTotals([
      row({ resourceId: "a", calls: 3, cost: 0.15 }),
      row({ resourceId: "b", metered: false, calls: 4, cost: 0 }),
    ]);
    expect(totals).toEqual({ resources: 2, calls: 7, cost: 0.15, currency: "USD" });
  });

  it("empty rows produce zero totals", () => {
    expect(resourceMeteringTotals([])).toEqual({ resources: 0, calls: 0, cost: 0, currency: "" });
  });
});
