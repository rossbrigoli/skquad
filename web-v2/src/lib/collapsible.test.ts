import { describe, expect, it } from "vitest";
import { collapseKey, initialExpanded, persistExpanded } from "./collapsible";

function memStore(initial: Record<string, string> = {}) {
  const map = new Map(Object.entries(initial));
  return {
    getItem: (k: string) => map.get(k) ?? null,
    setItem: (k: string, v: string) => void map.set(k, v),
    _map: map,
  };
}

describe("collapseKey", () => {
  it("namespaces by id", () => {
    expect(collapseKey("squad-recent-activity")).toBe("skquad:collapsible:squad-recent-activity");
  });
});

describe("initialExpanded", () => {
  it("is collapsed by default with no storage", () => {
    expect(initialExpanded("x", null)).toBe(false);
    expect(initialExpanded("x", undefined)).toBe(false);
  });

  it("is collapsed by default with empty storage", () => {
    expect(initialExpanded("x", memStore())).toBe(false);
  });

  it("is collapsed when another section was expanded but this one wasn't", () => {
    const s = memStore({ "skquad:collapsible:other": "1" });
    expect(initialExpanded("x", s)).toBe(false);
  });

  it("is expanded only when this section's key is exactly '1'", () => {
    const s = memStore({ "skquad:collapsible:x": "1" });
    expect(initialExpanded("x", s)).toBe(true);
  });

  it("stored value wins over fallback: '0' collapses even when fallback=true", () => {
    const s = memStore({ "skquad:collapsible:x": "0" });
    expect(initialExpanded("x", s, true)).toBe(false);
  });

  it("fallback applies only when no stored value exists", () => {
    expect(initialExpanded("x", memStore(), true)).toBe(true);
    expect(initialExpanded("x", null, true)).toBe(true);
    expect(initialExpanded("x", memStore({ "skquad:collapsible:x": "1" }), false)).toBe(true);
  });

  it("treats '0' or junk values as collapsed", () => {
    expect(initialExpanded("x", memStore({ "skquad:collapsible:x": "0" }))).toBe(false);
    expect(initialExpanded("x", memStore({ "skquad:collapsible:x": "yes" }))).toBe(false);
  });

  it("is collapsed when storage throws (private mode)", () => {
    const angry = {
      getItem: () => {
        throw new Error("blocked");
      },
      setItem: () => {},
    };
    expect(initialExpanded("x", angry)).toBe(false);
  });
});

describe("persistExpanded", () => {
  it("writes '1' when expanded and '0' when collapsed", () => {
    const s = memStore();
    persistExpanded(s, "x", true);
    expect(s._map.get("skquad:collapsible:x")).toBe("1");
    persistExpanded(s, "x", false);
    expect(s._map.get("skquad:collapsible:x")).toBe("0");
  });

  it("round-trips through initialExpanded", () => {
    const s = memStore();
    persistExpanded(s, "recent", true);
    expect(initialExpanded("recent", s)).toBe(true);
    persistExpanded(s, "recent", false);
    expect(initialExpanded("recent", s)).toBe(false);
  });

  it("swallows storage write errors (expansion still works in memory)", () => {
    const angry = {
      getItem: () => null,
      setItem: () => {
        throw new Error("quota");
      },
    };
    expect(() => persistExpanded(angry, "x", true)).not.toThrow();
  });

  it("is a no-op with null/undefined storage", () => {
    expect(() => persistExpanded(null, "x", true)).not.toThrow();
    expect(() => persistExpanded(undefined, "x", false)).not.toThrow();
  });
});
