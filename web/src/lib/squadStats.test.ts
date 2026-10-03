// S-211 item 10: the "Work in flight" tile counted blocked tasks as
// "open" (open was `tasks.length - done.length`). workInFlight fixes
// the bucketing: blocked is its own bucket and never counts as open.
import { describe, expect, it } from "vitest";
import { buildContextSavePayload, workInFlight } from "./squadStats";
import type { Task, TaskStatus } from "./api";

function task(status: TaskStatus, id = "t"): Task {
  return { id, board_id: "b", squad_id: "s", title: id, status };
}

describe("workInFlight (S-211)", () => {
  it("Ross's example: 1 done + 2 blocked → 0 open, 2 blocked", () => {
    const tasks = [task("done", "d1"), task("blocked", "b1"), task("blocked", "b2")];
    expect(workInFlight(tasks)).toEqual({ open: 0, blocked: 2, done: 1 });
  });

  it("blocked never counts toward open", () => {
    const tasks = [task("todo"), task("in-review"), task("blocked"), task("done")];
    const wip = workInFlight(tasks);
    expect(wip.blocked).toBe(1);
    expect(wip.open).toBe(2); // todo + in-review only
    expect(wip.open).not.toBe(3);
  });

  it("empty and null inputs are zeroed", () => {
    expect(workInFlight([])).toEqual({ open: 0, blocked: 0, done: 0 });
    expect(workInFlight(null)).toEqual({ open: 0, blocked: 0, done: 0 });
    expect(workInFlight(undefined)).toEqual({ open: 0, blocked: 0, done: 0 });
  });

  it("buckets partition the task list", () => {
    const tasks = [task("todo"), task("blocked"), task("blocked"), task("done"), task("in-review")];
    const wip = workInFlight(tasks);
    expect(wip.open + wip.blocked + wip.done).toBe(tasks.length);
  });
});

describe("buildContextSavePayload (S-211 items 2–4)", () => {
  it("saves mission and context together in one payload", () => {
    const payload = buildContextSavePayload("Ship the thing", "Conventions…");
    expect(payload).toEqual({ mission: "Ship the thing", prompt: "Conventions…" });
  });

  it("trims the mission but never merges it into the context text", () => {
    const payload = buildContextSavePayload("  Mission  ", "ctx body");
    expect(payload.mission).toBe("Mission");
    expect(payload.prompt).toBe("ctx body");
    expect(payload.prompt).not.toContain("Mission");
  });

  it("empty mission is preserved as empty (no injection upstream)", () => {
    expect(buildContextSavePayload("", "ctx")).toEqual({ mission: "", prompt: "ctx" });
  });
});
