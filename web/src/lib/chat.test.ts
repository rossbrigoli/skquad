import { describe, expect, it } from "vitest";
import type { Message } from "./api";
import {
  agentTurnPending,
  chatContextTokens,
  chatToolCalls,
  CHAT_TURN_LOCK_MS,
  formatContextTokens,
  isInterimChatReply,
  isTurnErrorMessage,
  parseSubagent,
  prettyToolArgs,
  sortChatMessages,
  subagentSummary,
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

// S-265: the runtime posts a terminal closure with payload.turn_error
// when the LLM call fails — renderers key the error styling off this.
describe("isTurnErrorMessage", () => {
  it("is true for an agent message flagged turn_error", () => {
    expect(
      isTurnErrorMessage(message({ from_type: "agent", payload: { message: "failed", turn_error: true } })),
    ).toBe(true);
  });

  it("is false for user messages, missing payloads, and non-true flags", () => {
    expect(isTurnErrorMessage(message({ from_type: "user", payload: { turn_error: true } }))).toBe(false);
    expect(isTurnErrorMessage(message({ from_type: "agent" }))).toBe(false);
    expect(isTurnErrorMessage(message({ from_type: "agent", payload: { message: "ok" } }))).toBe(false);
    expect(isTurnErrorMessage(message({ from_type: "agent", payload: { turn_error: "yes" } }))).toBe(false);
    expect(isTurnErrorMessage(message({ from_type: "agent", payload: { interim: true } }))).toBe(false);
  });
});

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

describe("agentTurnPending", () => {
  const now = Date.parse("2026-09-28T00:05:00Z");

  it("is false for an empty thread", () => {
    expect(agentTurnPending([], now)).toBe(false);
  });

  it("is false when the newest message is from the agent", () => {
    const msgs = [
      message({ id: "u1", from_type: "user", created_at: "2026-09-28T00:01:00Z" }),
      message({ id: "a1", from_type: "agent", created_at: "2026-09-28T00:02:00Z" }),
    ];
    expect(agentTurnPending(msgs, now)).toBe(false);
  });

  it("is true when the newest message is a recent user message", () => {
    const msgs = [
      message({ id: "a0", from_type: "agent", created_at: "2026-09-28T00:00:00Z" }),
      message({ id: "u1", from_type: "user", created_at: "2026-09-28T00:04:30Z" }),
    ];
    expect(agentTurnPending(msgs, now)).toBe(true);
  });

  it("expires once the user message is older than the lock window", () => {
    const msgs = [
      message({ id: "u1", from_type: "user", created_at: "2026-09-28T00:00:00Z" }),
    ];
    // 5 minutes after send = lock released (crashed-agent escape hatch).
    expect(agentTurnPending(msgs, now, CHAT_TURN_LOCK_MS)).toBe(false);
    // Just inside the window it stays locked.
    expect(agentTurnPending(msgs, now - 1000, CHAT_TURN_LOCK_MS)).toBe(true);
  });

  it("is false for missing or unparseable timestamps", () => {
    expect(agentTurnPending([message({ created_at: undefined })], now)).toBe(false);
    expect(agentTurnPending([message({ created_at: "not-a-date" })], now)).toBe(false);
  });

  // S-175: a cancelled turn must not keep the composer locked.
  it("is false when the newest user message was cancelled", () => {
    const msgs = [
      message({ id: "a0", from_type: "agent", created_at: "2026-09-28T00:00:00Z" }),
      message({ id: "u1", from_type: "user", status: "cancelled", created_at: "2026-09-28T00:04:30Z" }),
    ];
    expect(agentTurnPending(msgs, now)).toBe(false);
  });

  it("is false when the newest user message is dead or expired", () => {
    expect(
      agentTurnPending([message({ from_type: "user", status: "dead", created_at: "2026-09-28T00:04:30Z" })], now),
    ).toBe(false);
    expect(
      agentTurnPending([message({ from_type: "user", status: "expired", created_at: "2026-09-28T00:04:30Z" })], now),
    ).toBe(false);
  });

  it("stays true for pending/delivered user messages inside the window", () => {
    expect(
      agentTurnPending([message({ from_type: "user", status: "pending", created_at: "2026-09-28T00:04:30Z" })], now),
    ).toBe(true);
    expect(
      agentTurnPending([message({ from_type: "user", status: "delivered", created_at: "2026-09-28T00:04:30Z" })], now),
    ).toBe(true);
  });

  // S-262: multi-message turns — an interim progress reply does NOT end
  // the turn; the indicator/lock must survive the first agent chunk.
  it("is true when the newest message is a recent interim agent reply", () => {
    const msgs = [
      message({ id: "u1", from_type: "user", created_at: "2026-09-28T00:01:00Z" }),
      message({
        id: "a1",
        from_type: "agent",
        status: "sent",
        created_at: "2026-09-28T00:04:30Z",
        payload: { message: "still working…", interim: true },
      }),
    ];
    expect(agentTurnPending(msgs, now)).toBe(true);
  });

  it("releases the interim-reply lock once the interim note is older than the window", () => {
    const msgs = [
      message({
        id: "a1",
        from_type: "agent",
        status: "sent",
        created_at: "2026-09-28T00:00:00Z",
        payload: { message: "still working…", interim: true },
      }),
    ];
    // Dropped final reply: expires from the interim message like the user case.
    expect(agentTurnPending(msgs, now, CHAT_TURN_LOCK_MS)).toBe(false);
    expect(agentTurnPending(msgs, now - 1000, CHAT_TURN_LOCK_MS)).toBe(true);
  });

  it("is false when the newest agent reply is the turn_error closure", () => {
    const msgs = [
      message({ id: "u1", from_type: "user", created_at: "2026-09-28T00:01:00Z" }),
      message({
        id: "a1",
        from_type: "agent",
        status: "sent",
        created_at: "2026-09-28T00:04:50Z",
        payload: { message: "couldn't finish", turn_error: true },
      }),
    ];
    expect(agentTurnPending(msgs, now)).toBe(false);
  });

  it("releases when a newest interim agent reply is dead or cancelled", () => {
    const base = {
      from_type: "agent",
      created_at: "2026-09-28T00:04:30Z",
      payload: { message: "still working…", interim: true },
    };
    expect(agentTurnPending([message({ ...base, status: "dead" })], now)).toBe(false);
    expect(agentTurnPending([message({ ...base, status: "cancelled" })], now)).toBe(false);
  });

  it("re-pends from the newest interim when multiple interim replies exist", () => {
    const msgs = [
      message({
        id: "a1",
        from_type: "agent",
        status: "sent",
        created_at: "2026-09-28T00:00:30Z",
        payload: { message: "first progress note", interim: true },
      }),
      message({
        id: "a2",
        from_type: "agent",
        status: "sent",
        created_at: "2026-09-28T00:04:50Z",
        payload: { message: "second progress note", interim: true },
      }),
    ];
    // The later interim refreshes the window even though the first is stale.
    expect(agentTurnPending(msgs, now)).toBe(true);
  });
});

describe("isInterimChatReply", () => {
  it("is true only for agent messages with payload.interim === true", () => {
    expect(isInterimChatReply(message({ from_type: "agent", payload: { interim: true } }))).toBe(true);
    expect(isInterimChatReply(message({ from_type: "agent", payload: { message: "hi" } }))).toBe(false);
    expect(isInterimChatReply(message({ from_type: "agent", payload: undefined }))).toBe(false);
    expect(isInterimChatReply(message({ from_type: "agent", payload: { interim: "true" } }))).toBe(false);
    expect(isInterimChatReply(message({ from_type: "user", payload: { interim: true } }))).toBe(false);
  });
});

// S-163: subagent transparency parsing + summary.
describe("parseSubagent (S-163)", () => {
  it("parses a well-formed subagent payload", () => {
    const info = parseSubagent({
      thread: [
        { role: "user", content: "do it" },
        { role: "assistant", content: "thinking", tool_calls: [{ name: "exec", arguments: "{}" }] },
        { role: "tool", name: "exec", ok: true, content: "ran" },
      ],
      turns: 2,
      steps: ["exec"],
    });
    expect(info).not.toBeNull();
    expect(info!.turns).toBe(2);
    expect(info!.steps).toEqual(["exec"]);
    expect(info!.thread.map((e) => e.role)).toEqual(["user", "assistant", "tool"]);
    expect(info!.thread[1].tool_calls).toEqual([{ name: "exec", arguments: "{}" }]);
  });

  it("skips unknown roles and junk entries", () => {
    const info = parseSubagent({
      thread: [{ role: "banana", content: "x" }, "junk", { role: "notice", content: "dropped" }],
      turns: 1,
      steps: [],
    });
    expect(info!.thread).toEqual([{ role: "notice", content: "dropped" }]);
  });

  it("returns null for unusable payloads", () => {
    expect(parseSubagent(null)).toBeNull();
    expect(parseSubagent("nope")).toBeNull();
    expect(parseSubagent({ thread: [], steps: [] })).toBeNull();
  });

  it("chatToolCalls attaches subagent only when present", () => {
    const msg = message({
      from_type: "agent",
      payload: {
        message: "done",
        tool_calls: [
          { name: "echo", arguments: {}, ok: true, result: "e" },
          {
            name: "spawn_subagent",
            arguments: { task: "dig" },
            ok: true,
            result: "found it",
            subagent: { thread: [{ role: "user", content: "dig" }], turns: 3, steps: ["exec"] },
          },
        ],
      },
    });
    const calls = chatToolCalls(msg);
    expect(calls[0].subagent).toBeUndefined();
    expect(calls[1].subagent).toBeDefined();
    expect(calls[1].subagent!.turns).toBe(3);
  });
});

describe("subagentSummary (S-163)", () => {
  it("formats steps and turns", () => {
    expect(subagentSummary({ thread: [], turns: 4, steps: ["exec", "web_fetch", "read_file"] })).toBe(
      "3 steps (exec → web_fetch → read_file), 4 turns"
    );
  });

  it("handles singular and empty steps", () => {
    expect(subagentSummary({ thread: [], turns: 1, steps: ["exec"] })).toBe("1 step (exec), 1 turn");
    expect(subagentSummary({ thread: [], turns: 2, steps: [] })).toBe("no tool steps, 2 turns");
  });

  it("truncates long step lists", () => {
    const steps = ["a", "b", "c", "d", "e", "f", "g", "h"];
    const summary = subagentSummary({ thread: [], turns: 9, steps });
    expect(summary).toContain("8 steps");
    expect(summary).toContain("…");
  });
});
