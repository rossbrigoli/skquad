import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  formatCost,
  formatRelativeTime,
  leaseState,
  messageDeliveryNote,
  messageText,
  resourceLabel,
} from "../components/shared";
import type {
  AgentPermission,
  LLMProvider,
  Message,
  MeteringSummary,
  RegistryResource,
  Task,
} from "../lib/api";

const NOW = new Date("2026-09-03T12:00:00Z");

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.useRealTimers();
});

function msg(overrides: Partial<Message>): Message {
  return {
    id: "m-1",
    from_type: "agent",
    from_id: "a-1",
    to_agent_id: "a-2",
    squad_id: "s-1",
    type: "consult",
    status: "pending",
    ...overrides,
  };
}

describe("messageText", () => {
  it("prefers the payload message", () => {
    const message = msg({ payload: { message: "hello agent" } });
    expect(messageText(message)).toBe("hello agent");
  });

  it("falls back to serialising the payload", () => {
    const message = msg({ payload: { kind: "consult", ids: [1, 2] } });
    expect(messageText(message)).toBe(JSON.stringify({ kind: "consult", ids: [1, 2] }));
  });

  it("handles a missing payload", () => {
    expect(messageText(msg({}))).toBe("{}");
  });
});

describe("resourceLabel", () => {
  const providers = [{ id: "p-1", name: "OpenAI" }] as LLMProvider[];
  const resources = [{ id: "r-1", name: "Git Tool" }] as RegistryResource[];

  function permission(resource_type: string, resource_id: string): AgentPermission {
    return { id: "perm-1", agent_id: "a-1", resource_type, resource_id } as AgentPermission;
  }

  it("resolves LLM provider names from the provider list", () => {
    expect(resourceLabel(permission("llm_provider", "p-1"), providers, resources)).toBe("OpenAI");
  });

  it("resolves registry resource names from the resource list", () => {
    expect(resourceLabel(permission("tool", "r-1"), providers, resources)).toBe("Git Tool");
  });

  it("falls back to the raw id when the resource is unknown", () => {
    expect(resourceLabel(permission("llm_provider", "missing"), providers, resources)).toBe("missing");
    expect(resourceLabel(permission("skill", "missing"), providers, resources)).toBe("missing");
  });

  it("does not resolve a tool id against the provider list", () => {
    // p-1 exists as a provider id; a tool grant with the same id must not match.
    expect(resourceLabel(permission("tool", "p-1"), providers, resources)).toBe("p-1");
  });
});

describe("formatCost", () => {
  it("shows a placeholder when there is no summary", () => {
    expect(formatCost(null)).toBe("-");
  });

  it("defaults to USD and four decimal places", () => {
    const summary = { cost: 0.5 } as MeteringSummary;
    expect(formatCost(summary)).toBe("USD 0.5000");
  });

  it("treats a missing cost as zero", () => {
    expect(formatCost({ currency: "EUR" } as MeteringSummary)).toBe("EUR 0.0000");
  });
});

describe("leaseState", () => {
  function task(overrides: Partial<Task>): Task {
    return { id: "t-1", board_id: "b-1", squad_id: "s-1", title: "task", status: "in-progress", ...overrides } as Task;
  }

  it("is idle without an execution", () => {
    expect(leaseState(task({}))).toBe("idle");
  });

  it("is idle without a lease timestamp", () => {
    expect(leaseState(task({ execution_id: "e-1" }))).toBe("idle");
  });

  it("treats the Go zero time as no lease rather than a stalled one", () => {
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: "0001-01-01T00:00:00Z" }))).toBe("idle");
  });

  it("treats an unparseable timestamp as no lease", () => {
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: "not-a-date" }))).toBe("idle");
  });

  it("is running while the lease is in the future", () => {
    const future = new Date(NOW.getTime() + 60_000).toISOString();
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: future }))).toBe("running");
  });

  it("is stalled once the lease has lapsed", () => {
    const past = new Date(NOW.getTime() - 60_000).toISOString();
    expect(leaseState(task({ execution_id: "e-1", lease_expires_at: past }))).toBe("stalled");
  });
});

describe("formatRelativeTime", () => {
  it("returns an empty string for missing or invalid values", () => {
    expect(formatRelativeTime(undefined)).toBe("");
    expect(formatRelativeTime("")).toBe("");
    expect(formatRelativeTime("nonsense")).toBe("");
  });

  it("formats seconds, minutes, hours and days", () => {
    const iso = (offsetMs: number) => new Date(NOW.getTime() + offsetMs).toISOString();

    expect(formatRelativeTime(iso(45_000))).toMatch(/45 second/);
    expect(formatRelativeTime(iso(5 * 60_000))).toMatch(/5 minute/);
    expect(formatRelativeTime(iso(3 * 3_600_000))).toMatch(/3 hour/);
    expect(formatRelativeTime(iso(2 * 86_400_000))).toMatch(/2 day/);
  });

  it("distinguishes past from future", () => {
    const iso = (offsetMs: number) => new Date(NOW.getTime() + offsetMs).toISOString();
    const future = formatRelativeTime(iso(600_000));
    const past = formatRelativeTime(iso(-600_000));

    expect(future).not.toBe(past);
    expect(past).toMatch(/ago/);
  });
});

describe("messageDeliveryNote", () => {
  it("is empty for a message with no delivery metadata", () => {
    expect(messageDeliveryNote(msg({}))).toBe("");
  });

  it("omits zero attempts", () => {
    expect(messageDeliveryNote(msg({ attempts: 0 }))).toBe("");
  });

  it("includes the attempt count and max when known", () => {
    expect(messageDeliveryNote(msg({ attempts: 2 }))).toBe("attempt 2");
    expect(messageDeliveryNote(msg({ attempts: 2, max_attempts: 5 }))).toBe("attempt 2/5");
  });

  it("says a retry is due when the scheduled time already passed", () => {
    const past = new Date(NOW.getTime() - 30_000).toISOString();
    expect(messageDeliveryNote(msg({ next_retry_at: past }))).toBe("retry due");
  });

  it("shows a future retry as a relative time", () => {
    const future = new Date(NOW.getTime() + 120_000).toISOString();
    expect(messageDeliveryNote(msg({ next_retry_at: future }))).toMatch(/^retry .*minute/);
  });

  it("joins every part with a separator", () => {
    const expires = new Date(NOW.getTime() + 3_600_000).toISOString();
    const note = messageDeliveryNote(msg({ attempts: 1, max_attempts: 3, expires_at: expires }));

    expect(note).toMatch(/^attempt 1\/3/);
    expect(note).toContain("·");
    expect(note).toMatch(/expires .*hour/);
  });

  it("ignores an unparseable expiry rather than throwing", () => {
    const note = messageDeliveryNote(msg({ attempts: 1, expires_at: "garbage" }));
    expect(note).toContain("attempt 1");
  });
});
