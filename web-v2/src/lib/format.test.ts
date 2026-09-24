import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import {
  formatCost,
  formatMoney,
  formatRelativeTime,
  formatTokens,
  leaseState,
  messageDeliveryNote,
  messageText,
} from "./format";
import type { Message, MeteringSummary, Task } from "./api";

// Fixed clock so relative-time assertions never flake.
const NOW = new Date("2026-09-22T10:00:00Z");

beforeAll(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});

afterEach(() => {
  vi.setSystemTime(NOW);
});

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

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: "m1",
    from_type: "agent",
    from_id: "a1",
    to_agent_id: "a2",
    squad_id: "s1",
    type: "task.note",
    status: "pending",
    ...overrides,
  };
}

function iso(offsetMs: number): string {
  return new Date(NOW.getTime() + offsetMs).toISOString();
}

// Mirror the production compact formatter so assertions stay locale-consistent.
function compact(value: number): string {
  return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 }).format(value);
}

function relative(amount: number, unit: Intl.RelativeTimeFormatUnit): string {
  return new Intl.RelativeTimeFormat(undefined, { numeric: "auto" }).format(amount, unit);
}

describe("formatCost", () => {
  it("renders a dash when there is no summary", () => {
    expect(formatCost(null)).toBe("-");
  });

  it("formats cost with explicit currency to four decimals", () => {
    const summary: MeteringSummary = { cost: 1.23456, currency: "EUR" };
    expect(formatCost(summary)).toBe("EUR 1.2346");
  });

  it("defaults to USD when currency is empty or missing", () => {
    expect(formatCost({ cost: 0.5, currency: "" })).toBe("USD 0.5000");
    expect(formatCost({ cost: 0.5 })).toBe("USD 0.5000");
  });

  it("treats a missing cost as zero", () => {
    expect(formatCost({ currency: "USD" })).toBe("USD 0.0000");
  });

  it("shows significant digits for sub-tenth-of-cent costs instead of a misleading zero", () => {
    // 679 in + 112 out @ $0.01/$0.02 per 1M = $9.03e-06 — toFixed(4)
    // rendered this as "USD 0.0000", making the cost page look empty.
    expect(formatCost({ cost: 0.00000903, currency: "USD" })).toBe("USD 0.000009");
    expect(formatCost({ cost: 0.00001753, currency: "USD" })).toBe("USD 0.000018");
    expect(formatCost({ cost: 0.00005, currency: "USD" })).toBe("USD 0.00005");
  });

  it("keeps four-decimal rendering at or above a tenth of a cent", () => {
    expect(formatCost({ cost: 0.0001234, currency: "USD" })).toBe("USD 0.0001");
    expect(formatCost({ cost: 0.0099999, currency: "USD" })).toBe("USD 0.0100");
  });
});

describe("formatMoney", () => {
  it("formats zero with the fixed four-decimal shape", () => {
    expect(formatMoney(0)).toBe("USD 0.0000");
    expect(formatMoney(0, "EUR")).toBe("EUR 0.0000");
  });

  it("defaults to USD", () => {
    expect(formatMoney(1.5)).toBe("USD 1.5000");
  });

  it("uses two significant digits below a tenth of a cent", () => {
    expect(formatMoney(1.753e-05)).toBe("USD 0.000018");
    expect(formatMoney(-9.03e-06)).toBe("USD -0.000009");
  });

  it("does not leave a dangling decimal point after trimming", () => {
    expect(formatMoney(0.1)).toBe("USD 0.1000");
  });
});

describe("formatTokens", () => {
  it("says no usage recorded for null or zero-token summaries", () => {
    expect(formatTokens(null)).toBe("no usage recorded");
    expect(formatTokens({})).toBe("no usage recorded");
    expect(formatTokens({ input_tokens: 0, output_tokens: 0 })).toBe("no usage recorded");
  });

  it("formats input and output with compact notation", () => {
    const summary: MeteringSummary = { input_tokens: 12345, output_tokens: 678 };
    expect(formatTokens(summary)).toBe(`${compact(12345)} in · ${compact(678)} out`);
  });

  it("shows zero side explicitly when only one direction has usage", () => {
    expect(formatTokens({ input_tokens: 500 })).toBe(`${compact(500)} in · ${compact(0)} out`);
    expect(formatTokens({ output_tokens: 900 })).toBe(`${compact(0)} in · ${compact(900)} out`);
  });
});

describe("leaseState", () => {
  it("is idle without an execution id or lease expiry", () => {
    expect(leaseState(task())).toBe("idle");
    expect(leaseState(task({ execution_id: "e1" }))).toBe("idle");
    expect(leaseState(task({ lease_expires_at: iso(60_000) }))).toBe("idle");
  });

  it("treats Go zero times and unparseable values as no lease", () => {
    expect(leaseState(task({ execution_id: "e1", lease_expires_at: "0001-01-01T00:00:00Z" }))).toBe("idle");
    expect(leaseState(task({ execution_id: "e1", lease_expires_at: "not-a-date" }))).toBe("idle");
  });

  it("is running while the lease is still in the future", () => {
    expect(leaseState(task({ execution_id: "e1", lease_expires_at: iso(45_000) }))).toBe("running");
  });

  it("is stalled once the lease has expired", () => {
    expect(leaseState(task({ execution_id: "e1", lease_expires_at: iso(-1_000) }))).toBe("stalled");
  });
});

describe("formatRelativeTime", () => {
  it("returns empty string for missing or unparseable values", () => {
    expect(formatRelativeTime(undefined)).toBe("");
    expect(formatRelativeTime("")).toBe("");
    expect(formatRelativeTime("garbage")).toBe("");
  });

  it("buckets seconds, minutes, hours and days", () => {
    expect(formatRelativeTime(iso(30_000))).toBe(relative(30, "second"));
    expect(formatRelativeTime(iso(5 * 60_000))).toBe(relative(5, "minute"));
    expect(formatRelativeTime(iso(3 * 3_600_000))).toBe(relative(3, "hour"));
    expect(formatRelativeTime(iso(2 * 86_400_000))).toBe(relative(2, "day"));
  });

  it("handles past times as negative relative amounts", () => {
    expect(formatRelativeTime(iso(-10 * 60_000))).toBe(relative(-10, "minute"));
    expect(formatRelativeTime(iso(-3 * 86_400_000))).toBe(relative(-3, "day"));
  });

  it("rounds whole seconds before bucketing", () => {
    // 89s lands in the minute bucket and rounds to 1 minute; 89.5s first
    // becomes 90s via the seconds rounding, which is exactly 2 minutes.
    expect(formatRelativeTime(iso(89_000))).toBe(relative(1, "minute"));
    expect(formatRelativeTime(iso(89_500))).toBe(relative(2, "minute"));
  });
});

describe("messageText", () => {
  it("prefers the payload.message string", () => {
    expect(messageText(message({ payload: { message: "hello" } }))).toBe("hello");
  });

  it("falls back to JSON when there is no message string", () => {
    expect(messageText(message({ payload: { detail: 42 } }))).toBe(JSON.stringify({ detail: 42 }));
    expect(messageText(message())).toBe(JSON.stringify({}));
  });

  it("stringifies non-string message values via JSON fallback", () => {
    expect(messageText(message({ payload: { message: 7 } as never }))).toBe(JSON.stringify({ message: 7 }));
  });
});

describe("messageDeliveryNote", () => {
  it("is empty for a message with no retry/expiry metadata", () => {
    expect(messageDeliveryNote(message())).toBe("");
    expect(messageDeliveryNote(message({ attempts: 0 }))).toBe("");
  });

  it("shows attempt count with and without a max", () => {
    expect(messageDeliveryNote(message({ attempts: 2, max_attempts: 5 }))).toBe("attempt 2/5");
    expect(messageDeliveryNote(message({ attempts: 1 }))).toBe("attempt 1");
  });

  it("flags retry due when next_retry_at is in the past", () => {
    expect(messageDeliveryNote(message({ next_retry_at: iso(-5_000) }))).toBe("retry due");
  });

  it("shows a relative retry when next_retry_at is in the future", () => {
    const note = messageDeliveryNote(message({ next_retry_at: iso(120_000) }));
    expect(note).toBe(`retry ${relative(2, "minute")}`);
  });

  it("treats an unparseable next_retry_at as not-yet-due", () => {
    expect(messageDeliveryNote(message({ next_retry_at: "someday" }))).toBe("retry ");
  });

  it("joins attempts, retry and expiry parts with middots", () => {
    const note = messageDeliveryNote(
      message({ attempts: 1, max_attempts: 3, next_retry_at: iso(-1_000), expires_at: iso(600_000) }),
    );
    expect(note).toBe(`attempt 1/3 · retry due · expires ${relative(10, "minute")}`);
  });

  it("appends expiry on its own when there is no retry info", () => {
    expect(messageDeliveryNote(message({ expires_at: iso(300_000) }))).toBe(`expires ${relative(5, "minute")}`);
  });
});
