// TG-9 slice C: pure-logic tests for the audit drill-down helpers.
import { describe, expect, it } from "vitest";
import type { AuditEntry } from "./api";
import {
  AUDIT_LIMIT_DEFAULT,
  AUDIT_LIMIT_MAX,
  STABLE_DECISION_CODES,
  baseCode,
  buildAuditPath,
  clampAuditLimit,
  decisionLabel,
  deriveDecision,
  deriveTier,
  filterAuditEntries,
  formatActor,
  hasActiveFilters,
  parseAuditMetadata,
  taskRefFor,
} from "./audit";

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: overrides.id ?? `e${Math.random().toString(36).slice(2)}`,
    actor_type: "agent",
    actor_id: "ag1",
    action: "task.update",
    resource_type: "task",
    resource_id: "t1",
    timestamp: "2026-10-07T02:00:00Z",
    ...overrides,
  };
}

describe("STABLE_DECISION_CODES completeness", () => {
  // The stable code list mirrored from the backend (TG-8):
  // control-plane/internal/httpapi/confirmations.go,
  // tool-gateway/internal/httpapi/confirmationgate.go and
  // tool-gateway/internal/drivers/browser. If the backend adds a code,
  // this test fails until the mapping grows to match.
  const BACKEND_CODES = [
    "denied_replayed",
    "args_hash_mismatch",
    "approval_expired",
    "standing_grant_not_live",
    "denied_by_owner",
    "confirmation_expired",
    "confirmation_pending",
    "pending_confirmation",
    "session_invalid",
    "ceiling_exceeded",
    "browser_busy",
    "confirmation_unavailable",
  ];

  it.each(BACKEND_CODES)("maps %s to a human-readable label", (code) => {
    const mapped = STABLE_DECISION_CODES[code];
    expect(mapped).toBeDefined();
    expect(mapped.label).not.toBe(code);
    expect(mapped.label).toMatch(/[a-z]/);
    expect(["allow", "deny", "pending"]).toContain(mapped.kind);
  });

  it("has no extra codes beyond the backend list", () => {
    expect(Object.keys(STABLE_DECISION_CODES).sort()).toEqual([...BACKEND_CODES].sort());
  });

  it("renders the card-required examples human-readably", () => {
    expect(STABLE_DECISION_CODES.denied_replayed.label).toBe("already used");
    expect(STABLE_DECISION_CODES.ceiling_exceeded.label).toBe("limit exceeded");
    expect(STABLE_DECISION_CODES.session_invalid.label).toBe("session invalid");
    expect(STABLE_DECISION_CODES.pending_confirmation.label).toBe("awaiting owner approval");
  });
});

describe("baseCode + decisionLabel", () => {
  it("strips free-text detail after the colon", () => {
    expect(baseCode("denied_by_owner: not today")).toBe("denied_by_owner");
    expect(baseCode("denied_replayed")).toBe("denied_replayed");
    expect(baseCode("   ")).toBe("");
  });
  it("falls back to the underscore-spaced raw code when unknown", () => {
    expect(decisionLabel("mystery_code")).toBe("mystery code");
    expect(decisionLabel("")).toBe("allowed");
  });
});

describe("parseAuditMetadata", () => {
  it("passes objects through, parses JSON strings, rejects garbage", () => {
    expect(parseAuditMetadata(entry({ metadata: { gate: "auto" } }))).toEqual({ gate: "auto" });
    expect(parseAuditMetadata(entry({ metadata: '{"gate":"denied"}' }))).toEqual({ gate: "denied" });
    expect(parseAuditMetadata(entry({ metadata: "not json" }))).toBeNull();
    expect(parseAuditMetadata(entry({ metadata: undefined }))).toBeNull();
    expect(parseAuditMetadata(entry({ metadata: 42 as unknown as object }))).toBeNull();
  });
});

describe("deriveDecision", () => {
  it("maps gate outcomes", () => {
    expect(deriveDecision(entry({ metadata: { gate: "auto" } }))).toMatchObject({ kind: "allow", code: "auto" });
    expect(deriveDecision(entry({ metadata: { gate: "once" } }))).toMatchObject({ kind: "allow" });
    expect(deriveDecision(entry({ metadata: { gate: "standing" } }))).toMatchObject({ kind: "allow" });
    expect(deriveDecision(entry({ metadata: { gate: "pending" } }))).toMatchObject({
      kind: "pending",
      label: "awaiting owner approval",
    });
    expect(deriveDecision(entry({ metadata: { gate: "unavailable" } }))).toMatchObject({
      kind: "deny",
      code: "confirmation_unavailable",
    });
  });
  it("uses the stable reason code on a denied gate", () => {
    const d = deriveDecision(entry({ metadata: { gate: "denied", reason: "denied_replayed" } }));
    expect(d).toEqual({ kind: "deny", code: "denied_replayed", label: "already used" });
  });
  it("defaults a denied gate without reason to denied_by_owner", () => {
    expect(deriveDecision(entry({ metadata: { gate: "denied" } })).code).toBe("denied_by_owner");
  });
  it("passes unknown gate values through as deny with the raw code", () => {
    const d = deriveDecision(entry({ metadata: { gate: "weird_gate" } }));
    expect(d.kind).toBe("deny");
    expect(d.code).toBe("weird_gate");
    expect(d.label).toBe("weird gate");
  });
  it("maps confirmation action names", () => {
    expect(deriveDecision(entry({ action: "confirmation.approve_once" }))).toMatchObject({
      kind: "allow",
      code: "owner_approved",
    });
    expect(deriveDecision(entry({ action: "confirmation.approve_standing" })).kind).toBe("allow");
    expect(deriveDecision(entry({ action: "confirmation.standing_match" })).kind).toBe("allow");
    expect(
      deriveDecision(entry({ action: "confirmation.deny", metadata: { reason: "denied_by_owner: nope" } })),
    ).toEqual({ kind: "deny", code: "denied_by_owner", label: "denied by owner" });
  });
  it("maps a stable metadata.reason without a gate", () => {
    const d = deriveDecision(entry({ action: "browser.navigate", metadata: { reason: "ceiling_exceeded" } }));
    expect(d).toEqual({ kind: "deny", code: "ceiling_exceeded", label: "limit exceeded" });
  });
  it("treats *_denied actions as owner denials", () => {
    const d = deriveDecision(entry({ action: "task.workspace_link_denied" }));
    expect(d.kind).toBe("deny");
    expect(d.code).toBe("denied_by_owner");
  });
  it("treats plain mutations as allowed", () => {
    const d = deriveDecision(entry({ action: "task.create" }));
    expect(d).toEqual({ kind: "allow", code: "", label: "allowed" });
  });
});

describe("deriveTier / taskRefFor / formatActor", () => {
  it("reads risk_tier then tier", () => {
    expect(deriveTier(entry({ metadata: { risk_tier: "high" } }))).toBe("high");
    expect(deriveTier(entry({ metadata: { tier: "medium" } }))).toBe("medium");
    expect(deriveTier(entry())).toBe("");
  });
  it("resolves task refs from metadata or the resource itself", () => {
    expect(taskRefFor(entry({ metadata: { task_id: "t9" }, resource_type: "tool_resource" }))).toBe("t9");
    expect(taskRefFor(entry({ resource_type: "task", resource_id: "t3" }))).toBe("t3");
    expect(taskRefFor(entry({ resource_type: "agent", resource_id: "a1" }))).toBe("");
  });
  it("prefers actor_display, falls back per actor type", () => {
    expect(formatActor(entry({ actor_display: "Bob" }))).toBe("Bob");
    expect(formatActor(entry({ actor_type: "system", actor_id: "" }))).toBe("system");
    expect(formatActor(entry({ actor_id: "u-77" }))).toBe("u-77");
    expect(formatActor(entry({ actor_id: "", actor_type: "" }))).toBe("unknown");
  });
});

describe("clampAuditLimit", () => {
  it("defaults, floors and caps the window", () => {
    expect(clampAuditLimit(undefined)).toBe(AUDIT_LIMIT_DEFAULT);
    expect(clampAuditLimit(0)).toBe(AUDIT_LIMIT_DEFAULT);
    expect(clampAuditLimit(-5)).toBe(AUDIT_LIMIT_DEFAULT);
    expect(clampAuditLimit(NaN)).toBe(AUDIT_LIMIT_DEFAULT);
    expect(clampAuditLimit(150.7)).toBe(150);
    expect(clampAuditLimit(9999)).toBe(AUDIT_LIMIT_MAX);
  });
});

describe("buildAuditPath", () => {
  it("admin: global endpoint with optional squad filter and clamped limit", () => {
    expect(buildAuditPath({}, true)).toBe("/audit?limit=200");
    expect(buildAuditPath({ squadId: "s1" }, true)).toBe("/audit?squad_id=s1&limit=200");
    expect(buildAuditPath({ limit: 5000 }, true)).toBe("/audit?limit=500");
  });
  it("owner: squad-scoped endpoint, squad required", () => {
    expect(buildAuditPath({ squadId: "s1" }, false)).toBe("/squads/s1/audit?limit=200");
    expect(buildAuditPath({ squadId: "  " }, false)).toBe("");
    expect(buildAuditPath({}, false)).toBe("");
  });
  it("encodes the squad id in the owner path", () => {
    expect(buildAuditPath({ squadId: "a b/c" }, false)).toBe("/squads/a%20b%2Fc/audit?limit=200");
  });
});

describe("filterAuditEntries", () => {
  const rows = [
    entry({ id: "1", actor_type: "agent", actor_id: "ag1", action: "task.update", resource_id: "t1", timestamp: "2026-10-01T00:00:00Z" }),
    entry({ id: "2", actor_type: "user", actor_id: "u1", action: "confirmation.deny", resource_type: "tool_resource", resource_id: "r9", metadata: { task_id: "t1", reason: "denied_replayed" }, timestamp: "2026-10-03T00:00:00Z" }),
    entry({ id: "3", actor_type: "agent", actor_id: "ag2", action: "agent.update", resource_type: "agent", resource_id: "ag2", timestamp: "2026-10-05T00:00:00Z" }),
    entry({ id: "4", actor_type: "system", actor_id: "", action: "llm.metering.ingest", resource_type: "agent", resource_id: "ag1", timestamp: undefined }),
  ];

  it("no filters = everything passes", () => {
    expect(filterAuditEntries(rows, {})).toHaveLength(4);
  });
  it("filters by agent (agents only)", () => {
    expect(filterAuditEntries(rows, { agentId: "ag1" }).map((e) => e.id)).toEqual(["1"]);
  });
  it("filters by task (metadata or resource)", () => {
    expect(filterAuditEntries(rows, { taskId: "t1" }).map((e) => e.id)).toEqual(["1", "2"]);
  });
  it("filters by resource id", () => {
    expect(filterAuditEntries(rows, { resourceId: "r9" }).map((e) => e.id)).toEqual(["2"]);
  });
  it("filters by decision kind", () => {
    expect(filterAuditEntries(rows, { decision: "deny" }).map((e) => e.id)).toEqual(["2"]);
    expect(filterAuditEntries(rows, { decision: "allow" }).map((e) => e.id)).toEqual(["1", "3", "4"]);
  });
  it("filters by time range and drops untimed entries when a range is active", () => {
    const inRange = filterAuditEntries(rows, { since: "2026-10-02T00:00:00Z", until: "2026-10-04T00:00:00Z" });
    expect(inRange.map((e) => e.id)).toEqual(["2"]);
    expect(filterAuditEntries(rows, { since: "2026-10-04T00:00:00Z" }).map((e) => e.id)).toEqual(["3"]);
  });
  it("ignores unparseable bounds (filter inactive)", () => {
    expect(filterAuditEntries(rows, { since: "garbage" })).toHaveLength(4);
  });
  it("whitespace-only filters are inactive", () => {
    expect(filterAuditEntries(rows, { agentId: "  ", taskId: " " })).toHaveLength(4);
  });
});

describe("hasActiveFilters", () => {
  it("detects any non-empty filter", () => {
    expect(hasActiveFilters({})).toBe(false);
    expect(hasActiveFilters({ limit: 300 })).toBe(false);
    expect(hasActiveFilters({ agentId: " x " })).toBe(true);
    expect(hasActiveFilters({ decision: "deny" })).toBe(true);
    expect(hasActiveFilters({ until: "2026-10-01" })).toBe(true);
  });
});
