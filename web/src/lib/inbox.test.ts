import { describe, expect, it } from "vitest";
import {
  buildDeadLetterQuery,
  inboxIsEmpty,
  isConsultTimeout,
  retryLabel,
  type AgentInboxSnapshot,
  type InboxMessageRow,
} from "./inbox";

function row(overrides: Partial<InboxMessageRow> = {}): InboxMessageRow {
  return {
    id: "11111111-2222-3333-4444-555555555555",
    from_type: "agent",
    from_id: "aaaaaaaa-0000-0000-0000-000000000001",
    to_agent_id: "bbbbbbbb-0000-0000-0000-000000000002",
    squad_id: "cccccccc-0000-0000-0000-000000000003",
    type: "consult",
    status: "dead",
    attempts: 3,
    max_attempts: 3,
    created_at: "2026-09-29T00:00:00Z",
    ...overrides,
  };
}

describe("buildDeadLetterQuery", () => {
  it("returns empty string when no filters are set", () => {
    expect(buildDeadLetterQuery({})).toBe("");
    expect(buildDeadLetterQuery({ squad: "  ", reason: "" })).toBe("");
  });

  it("serializes only non-empty filters", () => {
    const qs = buildDeadLetterQuery({ squad: "abc", reason: " timeout ", limit: 25 });
    expect(qs).toContain("?squad=abc");
    expect(qs).toContain("reason=timeout");
    expect(qs).toContain("limit=25");
    expect(qs).not.toContain("agent=");
  });

  it("drops non-positive limits", () => {
    expect(buildDeadLetterQuery({ limit: 0 })).toBe("");
    expect(buildDeadLetterQuery({ limit: -5 })).toBe("");
  });
});

describe("retryLabel", () => {
  it("renders attempts against the max", () => {
    expect(retryLabel(row({ attempts: 2, max_attempts: 3 }))).toBe("attempt 2 of 3");
  });
});

describe("isConsultTimeout", () => {
  it("detects the terminal reason", () => {
    expect(isConsultTimeout(row({ terminal_reason: "consult_timeout" }))).toBe(true);
  });

  it("detects the payload marker on synthetic replies", () => {
    expect(isConsultTimeout(row({ status: "pending", payload: { consult_timeout: true } }))).toBe(true);
  });

  it("is false for ordinary dead letters", () => {
    expect(isConsultTimeout(row({ terminal_reason: "retry attempts exhausted" }))).toBe(false);
  });
});

describe("inboxIsEmpty", () => {
  const empty: AgentInboxSnapshot = {
    pending_count: 0,
    retrying_count: 0,
    delivered_count: 0,
    dead_count: 0,
    pending: [],
    retrying: [],
    delivered: [],
    dead: [],
  };

  it("treats null and all-zero snapshots as empty", () => {
    expect(inboxIsEmpty(null)).toBe(true);
    expect(inboxIsEmpty(empty)).toBe(true);
  });

  it("is not empty when any section has a count", () => {
    expect(inboxIsEmpty({ ...empty, dead_count: 1 })).toBe(false);
    expect(inboxIsEmpty({ ...empty, retrying_count: 2 })).toBe(false);
  });
});
