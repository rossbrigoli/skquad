import { describe, expect, it } from "vitest";
import type { Message, Task } from "./api";
import { taskResultInfo } from "./taskResult";

function task(overrides: Partial<Task> = {}): Task {
  return {
    id: "t1",
    board_id: "b1",
    squad_id: "s1",
    title: "task one",
    status: "todo",
    ...overrides,
  };
}

function reply(text: string, overrides: Partial<Message> = {}): Message {
  return {
    id: "m1",
    from_type: "agent",
    from_id: "a1",
    to_agent_id: "a1",
    squad_id: "s1",
    type: "reply",
    payload: { message: text },
    status: "delivered",
    created_at: "2026-09-30T01:00:00Z",
    ...overrides,
  };
}

describe("taskResultInfo (S-181)", () => {
  it("prefers the dedicated persisted result field", () => {
    const info = taskResultInfo(
      task({ status: "done", result: "  shipped it  ", result_status: "done", result_at: "2026-09-30T02:00:00Z" }),
      [reply("older thread text")],
    );
    expect(info).toEqual({ label: "Result", tone: "done", text: "shipped it", at: "2026-09-30T02:00:00Z" });
  });

  it("labels a blocked result as Blocked", () => {
    const info = taskResultInfo(
      task({ status: "blocked", result: "registry credentials expired", result_status: "blocked" }),
      [],
    );
    expect(info?.label).toBe("Blocked");
    expect(info?.tone).toBe("blocked");
    expect(info?.text).toBe("registry credentials expired");
  });

  it("falls back to the latest agent reply for pre-S-181 done tasks", () => {
    const info = taskResultInfo(task({ status: "done" }), [
      reply("first turn"),
      { ...reply("second turn"), id: "m2" },
    ]);
    expect(info?.text).toBe("second turn");
    expect(info?.tone).toBe("done");
    expect(info?.at).toBe("2026-09-30T01:00:00Z");
  });

  it("falls back to the latest agent reply for pre-S-181 blocked tasks", () => {
    const info = taskResultInfo(task({ status: "blocked" }), [
      reply("blocked because disk full", { id: "m2" }),
    ]);
    expect(info?.tone).toBe("blocked");
    expect(info?.text).toBe("blocked because disk full");
  });

  it("ignores non-reply and non-agent thread entries in the fallback", () => {
    const info = taskResultInfo(task({ status: "done" }), [
      reply("real answer", { id: "m1" }),
      { ...reply("noise", { id: "m2" }), from_type: "user" },
      { ...reply("noise", { id: "m3" }), type: "consult" },
    ]);
    expect(info?.text).toBe("real answer");
  });

  it("returns null for non-terminal tasks without a result", () => {
    expect(taskResultInfo(task({ status: "in-progress" }), [reply("wip")])).toBeNull();
  });

  it("returns null when a terminal task has no result and no agent reply text", () => {
    expect(taskResultInfo(task({ status: "done" }), [])).toBeNull();
    expect(taskResultInfo(task({ status: "done" }), [reply("   ")])).toBeNull();
  });
});
