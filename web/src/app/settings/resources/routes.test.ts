// S-204 follow-up: route-level source locks.
//
// Ross's bounce reported (1) breadcrumb links /settings/resources and
// /settings/resources/tools 404 and (2) no reliable way back from a
// tool config page. These locks keep the real routes and the real
// back-link in place (node env, no DOM — same style as the settings
// tab locks).
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

const indexSource = readFileSync(new URL("./page.tsx", import.meta.url), "utf8");
const toolsSource = readFileSync(new URL("./tools/page.tsx", import.meta.url), "utf8");
const configSource = readFileSync(
  new URL("./tools/[toolId]/page.tsx", import.meta.url),
  "utf8",
);
const categorySource = readFileSync(
  new URL("./[resourceType]/page.tsx", import.meta.url),
  "utf8",
);

describe("Resources index page (S-204 follow-up)", () => {
  it("links the category routes via the shared catalog", () => {
    expect(indexSource).toContain("RESOURCE_CATEGORIES");
    expect(indexSource).toContain("resourceCategoryHref");
    expect(indexSource).toContain("<AppShell>");
  });
});

describe("Tools tiles page (S-204 follow-up)", () => {
  it("renders the unified ToolsPanel on the canonical route", () => {
    expect(toolsSource).toContain("<ToolsPanel isAdmin={isAdmin} />");
    expect(toolsSource).toContain("ResourceModal");
  });
});

describe("Tool config page back-link (S-204 follow-up)", () => {
  it("uses a real Link to the Tools tiles page, not history.back()", () => {
    expect(configSource).toContain('href="/settings/resources/tools"');
    expect(configSource).toContain("Back to Tools");
    expect(configSource).not.toContain("window.history.back()");
  });

  it("returns to the Tools tiles page after delete", () => {
    expect(configSource).toContain('router.push("/settings/resources/tools")');
  });
});

describe("Resource category page (S-204 follow-up)", () => {
  it("validates the type against the catalog and 404s unknown types", () => {
    expect(categorySource).toContain("findResourceCategory");
    expect(categorySource).toContain("notFound()");
    expect(categorySource).toContain("<ResourceRegistryPanel");
  });
});
