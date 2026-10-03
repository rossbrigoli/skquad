import { describe, expect, it } from "vitest";
import type { TaskStatus } from "./api";
import {
  boardColumnsFromOperatingModel,
  CANONICAL_STATUSES,
  defaultColumns,
  moveColumn,
  normalizeColumns,
  parseOperatingModel,
  visibleColumns,
} from "./boardColumns";

describe("normalizeColumns", () => {
  it("returns canonical defaults for garbage input", () => {
    for (const bad of [undefined, null, "nope", 42, {}, "[]"]) {
      expect(normalizeColumns(bad)).toEqual(defaultColumns());
    }
  });

  it("keeps configured order, labels and visibility", () => {
    const cols = normalizeColumns([
      { status: "blocked", label: "Stuck", visible: false },
      { status: "todo", label: "Backlog" },
    ]);
    // S-213: the missing backlog column lands immediately left of todo.
    expect(cols.map((c) => c.status)).toEqual(["blocked", "backlog", "todo", "in-progress", "in-review", "done"]);
    expect(cols[0]).toEqual({ status: "blocked", label: "Stuck", visible: false });
    expect(cols[2].label).toBe("Backlog");
  });

  it("inserts backlog left of todo for pre-S-213 configs", () => {
    const cols = normalizeColumns([
      { status: "todo", label: "Now" },
      { status: "in-progress" },
      { status: "done" },
    ]);
    expect(cols.map((c) => c.status)).toEqual(["backlog", "todo", "in-progress", "done", "in-review", "blocked"]);
    expect(cols[0]).toEqual({ status: "backlog", label: "Backlog", visible: true });
  });

  it("keeps backlog immediately left of todo even when the config starts elsewhere", () => {
    const cols = normalizeColumns([{ status: "in-progress" }, { status: "done" }]);
    const backlogIdx = cols.findIndex((c) => c.status === "backlog");
    const todoIdx = cols.findIndex((c) => c.status === "todo");
    expect(backlogIdx).toBeGreaterThanOrEqual(0);
    expect(backlogIdx).toBe(todoIdx - 1);
  });

  it("respects a hidden backlog instead of re-adding it", () => {
    const cols = normalizeColumns([{ status: "backlog", visible: false }, { status: "todo" }]);
    expect(cols.map((c) => c.status)).toEqual(["backlog", "todo", "in-progress", "in-review", "done", "blocked"]);
    expect(cols[0].visible).toBe(false);
  });

  it("drops unknown statuses and duplicates", () => {
    const cols = normalizeColumns([
      { status: "todo" },
      { status: "icebox" },
      { status: "todo", label: "Again" },
    ]);
    expect(cols.map((c) => c.status)).toEqual(CANONICAL_STATUSES);
    expect(cols.find((c) => c.status === "todo")?.label).toBe("To do");
  });

  it("falls back to default label for blank labels", () => {
    const cols = normalizeColumns([{ status: "done", label: "   " }]);
    expect(cols.find((c) => c.status === "done")?.label).toBe("Done");
  });
});

describe("parseOperatingModel", () => {
  it("handles object, JSON string, and junk", () => {
    expect(parseOperatingModel({ a: 1 })).toEqual({ a: 1 });
    expect(parseOperatingModel('{"a": 1}')).toEqual({ a: 1 });
    expect(parseOperatingModel("[1,2]")).toEqual({});
    expect(parseOperatingModel("{broken")).toEqual({});
    expect(parseOperatingModel(null)).toEqual({});
  });
});

describe("boardColumnsFromOperatingModel", () => {
  it("reads nested board.columns", () => {
    const cols = boardColumnsFromOperatingModel({
      board: { columns: [{ status: "in-review", label: "QA" }] },
    });
    expect(cols[0]).toEqual({ status: "in-review", label: "QA", visible: true });
    expect(cols).toHaveLength(6);
  });

  it("defaults when board or columns missing", () => {
    expect(boardColumnsFromOperatingModel({})).toEqual(defaultColumns());
    expect(boardColumnsFromOperatingModel({ board: {} })).toEqual(defaultColumns());
    expect(boardColumnsFromOperatingModel({ board: 7 })).toEqual(defaultColumns());
  });
});

describe("visibleColumns / moveColumn", () => {
  it("filters hidden columns", () => {
    const cols = defaultColumns().map((c) => ({ ...c, visible: c.status !== "blocked" }));
    expect(visibleColumns(cols).map((c) => c.status)).toEqual(["backlog", "todo", "in-progress", "in-review", "done"]);
  });

  it("moves columns without mutating the input", () => {
    const cols = defaultColumns();
    const next = moveColumn(cols, "done", -1);
    expect(next.map((c) => c.status)).toEqual(["backlog", "todo", "in-progress", "done", "in-review", "blocked"]);
    expect(cols.map((c) => c.status)).toEqual(CANONICAL_STATUSES);
    expect(moveColumn(cols, "backlog", -1)).toEqual(cols);
    expect(moveColumn(cols, "nope" as TaskStatus, 1)).toEqual(cols);
  });
});
