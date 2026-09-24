import { describe, expect, it } from "vitest";
import type { Message } from "./api";
import {
  chatContextTokens,
  chatToolCalls,
  formatContextTokens,
  prettyToolArgs,
  sortChatMessages,
  summarizeToolArgs,
  truncateText,
} from "./chat";

function message(overrides: Partial<Message> = {}): Message {
  return {
    id: "m-1",
    from_type: "user",
    from_id: "u-1",
    to_agent_id: "a-1",
    squad_id: "s-1",
    type: "consult",
    status: "pending",
    created_at: "2026-09-25T00:00:00Z",
    ...overrides,
  };
}

describe("sortChatMessages", () => {
  it("sorts chronologically (oldest first)", () => {
    const msgs = [
      message({ id: "c", created_at: "2026-09-25T03:00:00Z" }),
      message({ id: "a", created_at: "2026-09-25T01:00:00Z" }),
      message({ id: "b", created_at: "2026-09-25T02:00:00Z" }),
    ];
    expect(sortChatMessages(msgs).map((m) => m.id)).toEqual(["a", "b", "c"]);
  });

  it("does not mutate the input array", () => {
    const msgs = [
      message({ id: "c" }),
      message({ id: "a", created_at: "2026-01-01T00:00:00Z" }),
    ];
    const original = [...msgs];
    sortChatMessages(msgs);
    expect(msgs).toEqual(original);
  });

  it("tolerates missing created_at", () => {
    const msgs = [
      message({ id: "x", created_at: undefined }),
      message({ id: "y", created_at: "2026-01-01T00:00:00Z" }),
    ];
    expect(sortChatMessages(msgs).map((m) => m.id)).toEqual(["x", "y"]);
  });
});

describe("chatToolCalls", () => {
  it("parses well-formed tool calls", () => {
    const msg = message({
      from_type: "agent",
      payload: {
        message: "done",
        tool_calls: [
          { name: "echo", arguments: { message: "hi" }, ok: true, result: "echo: hi" },
        ],
      },
    });
    expect(chatToolCalls(msg)).toEqual([
      { name: "echo", arguments: { message: "hi" }, ok: true, result: "echo: hi" },
    ]);
  });

  it("returns empty for missing or non-array payloads", () => {
    expect(chatToolCalls(message({ payload: undefined }))).toEqual([]);
    expect(chatToolCalls(message({ payload: { tool_calls: "nope" } }))).toEqual([]);
    expect(chatToolCalls(message({ payload: {} }))).toEqual([]);
  });

  it("skips malformed entries without throwing", () => {
    const msg = message({
      payload: { tool_calls: [null, 42, "string", [], { name: "ok" }] },
    });
    const calls = chatToolCalls(msg);
    expect(calls).toHaveLength(1);
    expect(calls[0]).toEqual({ name: "ok", arguments: {}, ok: true, result: "" });
  });

  it("defaults blank names to 'tool' and marks ok:false only when explicitly false", () => {
    const msg = message({
      payload: {
        tool_calls: [
          { name: "  ", arguments: { a: 1 }, ok: false, result: "boom" },
          { name: "fine", ok: "yes" },
        ],
      },
    });
    const calls = chatToolCalls(msg);
    expect(calls[0].name).toBe("tool");
    expect(calls[0].ok).toBe(false);
    expect(calls[1].ok).toBe(true);
  });
});

describe("chatContextTokens", () => {
  it("returns the newest agent reply's context_tokens", () => {
    const msgs = [
      message({ id: "u1", from_type: "user", created_at: "2026-09-25T01:00:00Z" }),
      message({
        id: "a1",
        from_type: "agent",
        created_at: "2026-09-25T01:00:10Z",
        payload: { message: "first", context_tokens: 100 },
      }),
      message({
        id: "a2",
        from_type: "agent",
        created_at: "2026-09-25T02:00:00Z",
        payload: { message: "second", context_tokens: 2500 },
      }),
    ];
    expect(chatContextTokens(msgs)).toBe(2500);
  });

  it("ignores user messages even when newer", () => {
    const msgs = [
      message({
        id: "a1",
        from_type: "agent",
        created_at: "2026-09-25T01:00:00Z",
        payload: { context_tokens: 300 },
      }),
      message({
        id: "u2",
        from_type: "user",
        created_at: "2026-09-25T05:00:00Z",
        payload: { context_tokens: 9999 },
      }),
    ];
    expect(chatContextTokens(msgs)).toBe(300);
  });

  it("returns null when no agent message carries usable tokens", () => {
    expect(chatContextTokens([])).toBeNull();
    expect(chatContextTokens([message({ from_type: "agent", payload: {} })])).toBeNull();
    expect(
      chatContextTokens([
        message({ from_type: "agent", payload: { context_tokens: "many" } }),
      ]),
    ).toBeNull();
    expect(
      chatContextTokens([message({ from_type: "agent", payload: { context_tokens: -1 } })]),
    ).toBeNull();
    expect(
      chatContextTokens([
        message({ from_type: "agent", payload: { context_tokens: Number.NaN } }),
      ]),
    ).toBeNull();
  });

  it("accepts zero (a tiny context is still a context)", () => {
    expect(
      chatContextTokens([message({ from_type: "agent", payload: { context_tokens: 0 } })]),
    ).toBe(0);
  });
});

describe("truncateText", () => {
  it("leaves short text alone", () => {
    expect(truncateText("hello", 10)).toBe("hello");
  });

  it("truncates with an ellipsis", () => {
    expect(truncateText("hello world", 5)).toBe("hello…");
  });

  it("non-positive max returns the value unchanged", () => {
    expect(truncateText("hello", 0)).toBe("hello");
  });
});

describe("summarizeToolArgs", () => {
  it("renders compact JSON", () => {
    expect(summarizeToolArgs({ message: "hi" })).toBe('{"message":"hi"}');
  });

  it("returns empty string for empty objects / null", () => {
    expect(summarizeToolArgs({})).toBe("");
    expect(summarizeToolArgs(null)).toBe("");
  });

  it("collapses whitespace and truncates long strings", () => {
    const summary = summarizeToolArgs({ q: "a   b\n\tc".padEnd(200, "z") }, 20);
    expect(summary.length).toBeLessThanOrEqual(21); // 20 chars + ellipsis
    expect(summary.endsWith("…")).toBe(true);
  });

  it("handles circular structures without throwing", () => {
    const circular: Record<string, unknown> = {};
    circular.self = circular;
    expect(() => summarizeToolArgs(circular)).not.toThrow();
  });
});

describe("prettyToolArgs", () => {
  it("pretty-prints JSON with indentation", () => {
    expect(prettyToolArgs({ a: 1 })).toBe('{\n  "a": 1\n}');
  });

  it("bounds very large payloads", () => {
    const big = { data: "x".repeat(5000) };
    expect(prettyToolArgs(big, 100).length).toBeLessThanOrEqual(101);
  });
});

describe("formatContextTokens", () => {
  it("groups thousands", () => {
    expect(formatContextTokens(1234567)).toBe("1,234,567");
  });
});
