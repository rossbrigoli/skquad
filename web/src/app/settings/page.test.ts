// S-204: the separate "Built-in Tools" settings screen must be gone —
// built-ins are managed on the unified Resources > Tools panel.
// Source-level lock (same style as the S-205 transport locks).
//
// S-204 follow-up: Resources moved off the in-page tab onto real routes
// (/settings/resources + /settings/resources/<type>) so breadcrumb
// levels resolve; the Settings "Resources" tab now navigates there.
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

  it("navigates to the real Resources index instead of an in-page tab", () => {
    expect(source).toContain('router.push("/settings/resources")');
    expect(source).not.toContain("ResourcesTab");
    expect(source).not.toContain("RESOURCE_TABS");
  });
});
