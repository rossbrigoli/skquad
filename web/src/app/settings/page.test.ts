// S-204: the separate "Built-in Tools" settings screen must be gone —
// built-ins are managed on the unified Resources > Tools panel.
// Source-level lock (same style as the S-205 transport locks).
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const source = readFileSync(new URL("./page.tsx", import.meta.url), "utf8");

describe("Settings page tab surface (S-204)", () => {
  it("has no builtin-tools tab", () => {
    expect(source).not.toContain('"builtin-tools"');
    expect(source).not.toContain("Built-in Tools");
  });

  it("no longer imports the old BuiltinToolsPanel", () => {
    expect(source).not.toContain("BuiltinToolsPanel");
  });

  it("renders the unified ToolsPanel for the tools resource tab", () => {
    expect(source).toContain('<ToolsPanel isAdmin={isAdmin} />');
    expect(source).toContain('active.key === "tools"');
  });
});
