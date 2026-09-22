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
    expect(cols.map((c) => c.status)).toEqual(["blocked", "todo", "in-progress", "in-review", "done"]);
    expect(cols[0]).toEqual({ status: "blocked", label: "Stuck", visible: false });
    expect(cols[1].label).toBe("Backlog");
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
    expect(cols).toHaveLength(5);
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
    expect(visibleColumns(cols).map((c) => c.status)).toEqual(["todo", "in-progress", "in-review", "done"]);
  });

  it("moves columns without mutating the input", () => {
    const cols = defaultColumns();
    const next = moveColumn(cols, "done", -1);
    expect(next.map((c) => c.status)).toEqual(["todo", "in-progress", "done", "in-review", "blocked"]);
    expect(cols.map((c) => c.status)).toEqual(CANONICAL_STATUSES);
    expect(moveColumn(cols, "todo", -1)).toEqual(cols);
    expect(moveColumn(cols, "nope" as TaskStatus, 1)).toEqual(cols);
  });
});
