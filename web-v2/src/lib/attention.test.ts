import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";
import { DEFAULT_STALE_REVIEW_MS, REASON_PRIORITY, buildAttention, type AttentionInput } from "./attention";
import type { Agent, InboxMessage, Task } from "./api";

const NOW = Date.parse("2026-09-22T12:00:00Z");

// leaseState() reads the real clock; pin it so "stalled" is deterministic.
beforeAll(() => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW);
});
afterAll(() => {
  vi.useRealTimers();
});

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

function agent(overrides: Partial<Agent> = {}): Agent {
  return {
    id: "a1",
    squad_id: "s1",
    name: "worker-one",
    ...overrides,
  } as Agent;
}

function msg(overrides: Partial<InboxMessage> = {}): InboxMessage {
  return {
    id: "m1",
    squad_id: "s1",
    kind: "task_completed",
    message: "all done",
    created_at: new Date(NOW - 60_000).toISOString(),
    ...overrides,
  };
}

function input(overrides: Partial<AttentionInput> = {}): AttentionInput {
  return { tasksBySquad: {}, agents: [], inbox: [], now: NOW, ...overrides };
}

const iso = (offsetMs: number) => new Date(NOW + offsetMs).toISOString();

describe("buildAttention ordering", () => {
  it("sorts by reason priority first, newest first within a reason", () => {
    const items = buildAttention(
      input({
        tasksBySquad: {
          s1: [
            task({ id: "review-old", status: "in-review", updated_at: iso(-2 * 86_400_000) }),
            task({ id: "blocked-1", status: "blocked", updated_at: iso(-1000) }),
            task({
              id: "stalled-1",
              status: "in-progress",
              execution_id: "e1",
              lease_expires_at: iso(-5_000),
              updated_at: iso(-2000),
            }),
          ],
        },
        agents: [agent({ id: "aErr", status: "error" })],
        inbox: [
          msg({ id: "mDone", kind: "task_completed" }),
          msg({ id: "mAct", kind: "action_required", created_at: iso(-30_000) }),
        ],
      }),
    );
    expect(items.map((i) => i.reason)).toEqual([
      "stalled",
      "agent_error",
      "blocked",
      "action_required",
      "stale_review",
      "completed",
    ]);
  });

  it("breaks priority ties with newest-first timestamps", () => {
    const items = buildAttention(
      input({
        inbox: [
          msg({ id: "old", created_at: iso(-7_200_000) }),
          msg({ id: "fresh", created_at: iso(-60_000) }),
        ],
      }),
    );
    expect(items.map((i) => i.id)).toEqual(["done:fresh", "done:old"]);
  });
});

describe("buildAttention task rules", () => {
  it("flags stalled leases with assignee name", () => {
    const items = buildAttention(
      input({
        tasksBySquad: {
          s1: [
            task({
              id: "tStall",
              status: "in-progress",
              assignee_agent_id: "a9",
              execution_id: "e1",
              lease_expires_at: iso(-1),
            }),
          ],
        },
        agentName: (id?: string) => (id === "a9" ? "rusty" : undefined),
      }),
    );
    expect(items).toHaveLength(1);
    expect(items[0].reason).toBe("stalled");
    expect(items[0].meta).toContain("rusty");
    expect(items[0].href).toBe("/squads/s1/tasks/tStall");
  });

  it("does not flag running leases", () => {
    const items = buildAttention(
      input({
        tasksBySquad: {
          s1: [task({ status: "in-progress", execution_id: "e1", lease_expires_at: iso(60_000) })],
        },
      }),
    );
    expect(items).toHaveLength(0);
  });

  it("flags blocked tasks even without a lease", () => {
    const items = buildAttention(input({ tasksBySquad: { s1: [task({ status: "blocked" })] } }));
    expect(items.map((i) => i.reason)).toEqual(["blocked"]);
  });

  it("flags in-review tasks only past the stale threshold", () => {
    const justUnder = buildAttention(
      input({
        tasksBySquad: { s1: [task({ status: "in-review", updated_at: iso(-DEFAULT_STALE_REVIEW_MS + 60_000) })] },
      }),
    );
    expect(justUnder).toHaveLength(0);

    const stale = buildAttention(
      input({ tasksBySquad: { s1: [task({ status: "in-review", updated_at: iso(-DEFAULT_STALE_REVIEW_MS) })] } }),
    );
    expect(stale).toHaveLength(1);
    expect(stale[0].meta).toContain("1d");
  });

  it("honours a custom stale threshold", () => {
    const items = buildAttention(
      input({
        tasksBySquad: { s1: [task({ status: "in-review", updated_at: iso(-61_000) })] },
        staleReviewAfterMs: 60_000,
      }),
    );
    expect(items).toHaveLength(1);
    expect(items[0].meta).toContain("over a day");
  });

  it("ignores in-review tasks with missing or unparseable updated_at", () => {
    const items = buildAttention(
      input({
        tasksBySquad: {
          s1: [task({ id: "noTime", status: "in-review" }), task({ id: "badTime", status: "in-review", updated_at: "nope" })],
        },
      }),
    );
    expect(items).toHaveLength(0);
  });
});

describe("buildAttention agents", () => {
  it("flags error and failed agents, ignores healthy ones", () => {
    const items = buildAttention(
      input({
        agents: [
          agent({ id: "a1", status: "error" }),
          agent({ id: "a2", status: "FAILED" }),
          agent({ id: "a3", status: "idle" }),
          agent({ id: "a4" }),
        ],
      }),
    );
    expect(items.map((i) => i.id).sort()).toEqual(["agent_error:a1", "agent_error:a2"]);
    expect(items[0].href).toBe("/squads/s1/agents/a1");
  });
});

describe("buildAttention inbox", () => {
  it("skips read messages entirely", () => {
    const items = buildAttention(
      input({ inbox: [msg({ id: "read1", read_at: iso(-1) }), msg({ id: "unread1" })] }),
    );
    expect(items.map((i) => i.id)).toEqual(["done:unread1"]);
  });

  it("deep-links action_required to the task when present, squad otherwise", () => {
    const items = buildAttention(
      input({
        inbox: [
          msg({ id: "withTask", kind: "action_required", task_id: "t77" }),
          msg({ id: "noTask", kind: "action_required" }),
        ],
      }),
    );
    const byId = Object.fromEntries(items.map((i) => [i.id, i.href]));
    expect(byId["action:withTask"]).toBe("/squads/s1/tasks/t77");
    expect(byId["action:noTask"]).toBe("/squads/s1");
  });

  it("uses the default agent-name fallback when no name resolver is given", () => {
    const items = buildAttention(
      input({
        tasksBySquad: {
          s1: [
            task({
              id: "tX",
              status: "in-progress",
              assignee_agent_id: "abcdef123456",
              execution_id: "e1",
              lease_expires_at: iso(-1),
            }),
          ],
        },
      }),
    );
    expect(items[0].meta).toContain("abcdef12");
  });

  it("keeps the priority map exhaustive over reasons", () => {
    expect(Object.keys(REASON_PRIORITY)).toHaveLength(6);
  });
});
