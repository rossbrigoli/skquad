// S-204 follow-up: resource category catalog tests.
import { describe, expect, it } from "vitest";

import {
  findResourceCategory,
  RESOURCE_CATEGORIES,
  resourceCategoryHref,
} from "./resourceCategories";

describe("resourceCategories (S-204 follow-up)", () => {
  it("contains the five known categories in display order", () => {
    expect(RESOURCE_CATEGORIES.map((c) => c.key)).toEqual([
      "skills",
      "tools",
      "apis",
      "knowledge-bases",
      "project-workspaces",
    ]);
  });

  it("finds a category by key", () => {
    expect(findResourceCategory("tools")?.label).toBe("Tools");
    expect(findResourceCategory("knowledge-bases")?.label).toBe("Knowledge bases");
  });

  it("returns undefined for unknown or missing keys", () => {
    expect(findResourceCategory("nope")).toBeUndefined();
    expect(findResourceCategory(undefined)).toBeUndefined();
    expect(findResourceCategory("")).toBeUndefined();
  });

  it("builds encoded category hrefs under /settings/resources", () => {
    expect(resourceCategoryHref("tools")).toBe("/settings/resources/tools");
    expect(resourceCategoryHref("knowledge bases")).toBe(
      "/settings/resources/knowledge%20bases",
    );
  });
});
