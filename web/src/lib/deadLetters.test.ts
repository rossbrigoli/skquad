// S-227: unit tests for the dead-letter bulk-selection helpers —
// select-all / none / indeterminate inputs, pruning against the live
// list, and the allSettled bulk runner + summary used by the panel.
import { describe, expect, it, vi } from "vitest";
import {
  pruneDeadLetterSelection,
  runBulkAction,
  allDeadLetterIds,
  emptyDeadLetterSelection,
  summarizeBulkResults,
  toggleDeadLetterSelection,
} from "./deadLetters";

const items = [{ id: "a" }, { id: "b" }, { id: "c" }];

describe("toggleDeadLetterSelection", () => {
  it("adds an unselected id", () => {
    const next = toggleDeadLetterSelection(new Set(["a"]), "b");
    expect([...next].sort()).toEqual(["a", "b"]);
  });

  it("removes a selected id", () => {
    const next = toggleDeadLetterSelection(new Set(["a", "b"]), "a");
    expect([...next]).toEqual(["b"]);
  });

  it("does not mutate the input set", () => {
    const orig = new Set(["a"]);
    toggleDeadLetterSelection(orig, "b");
    expect([...orig]).toEqual(["a"]);
  });
});

describe("allDeadLetterIds / emptyDeadLetterSelection", () => {
  it("returns every id when true", () => {
    expect([...allDeadLetterIds(items)].sort()).toEqual(["a", "b", "c"]);
  });

  it("returns an empty set when false", () => {
    expect(emptyDeadLetterSelection().size).toBe(0);
  });

  it("empty list + true is still empty (select-all stays off)", () => {
    expect(allDeadLetterIds([]).size).toBe(0);
  });
});

describe("pruneDeadLetterSelection", () => {
  it("drops ids no longer in the live list", () => {
    const pruned = pruneDeadLetterSelection(new Set(["a", "b", "gone"]), items);
    expect([...pruned].sort()).toEqual(["a", "b"]);
  });

  it("keeps a selection that fully exists", () => {
    expect([...pruneDeadLetterSelection(new Set(["c"]), items)]).toEqual(["c"]);
  });

  it("empties the selection when the list is empty (post-delete refresh)", () => {
    expect(pruneDeadLetterSelection(new Set(["a"]), []).size).toBe(0);
  });
});

describe("summarizeBulkResults", () => {
  it("counts successes and failures with failed ids", () => {
    const s = summarizeBulkResults([
      { id: "a", ok: true },
      { id: "b", ok: false, error: "boom" },
      { id: "c", ok: true },
      { id: "d", ok: false, error: "bang" },
    ]);
    expect(s).toEqual({ total: 4, succeeded: 2, failed: 2, failedIds: ["b", "d"] });
  });

  it("all-ok batch reports zero failures", () => {
    const s = summarizeBulkResults([{ id: "a", ok: true }]);
    expect(s.failed).toBe(0);
    expect(s.failedIds).toEqual([]);
  });

  it("empty batch is zero/zero", () => {
    expect(summarizeBulkResults([])).toEqual({ total: 0, succeeded: 0, failed: 0, failedIds: [] });
  });
});

describe("runBulkAction", () => {
  it("marks every item ok when all calls resolve", async () => {
    const results = await runBulkAction(["a", "b"], async () => undefined);
    expect(results).toEqual([
      { id: "a", ok: true },
      { id: "b", ok: true },
    ]);
  });

  it("captures per-item errors without rejecting the batch", async () => {
    const results = await runBulkAction(["a", "b", "c"], async (id) => {
      if (id === "b") throw new Error("404 not found");
      return id;
    });
    expect(results).toEqual([
      { id: "a", ok: true },
      { id: "b", ok: false, error: "404 not found" },
      { id: "c", ok: true },
    ]);
  });

  it("calls the function exactly once per id", async () => {
    const fn = vi.fn(async (_id: string) => undefined);
    await runBulkAction(["x", "y", "z"], fn);
    expect(fn).toHaveBeenCalledTimes(3);
    expect(fn.mock.calls.map((c) => c[0])).toEqual(["x", "y", "z"]);
  });

  it("stringifies non-Error rejections", async () => {
    const results = await runBulkAction(["a"], async () => {
      throw "weird";
    });
    expect(results[0]).toEqual({ id: "a", ok: false, error: "weird" });
  });
});
