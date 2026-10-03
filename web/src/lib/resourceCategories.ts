// S-204 follow-up: resource category catalog for the real
// /settings/resources route tree.
//
// Before this change the Resources surface lived only as a client-side
// tab inside /settings, so breadcrumb links like /settings/resources and
// /settings/resources/tools 404'd. These categories back both the
// Resources index page and the generic /settings/resources/[type]
// page, and the list is the allowlist the dynamic route validates
// against (unknown types render a 404, not an arbitrary registry call).

export type ResourceCategory = {
  readonly key: string;
  readonly label: string;
  readonly description: string;
};

// RESOURCE_CATEGORIES is the canonical ordering shown on the
// /settings/resources index (mirrors the old Settings tab order).
export const RESOURCE_CATEGORIES: readonly ResourceCategory[] = [
  { key: "skills", label: "Skills", description: "Reusable capabilities agents can install." },
  { key: "tools", label: "Tools", description: "Built-in and registered tools — pick a tool to configure it." },
  { key: "apis", label: "APIs", description: "External API resources agents may be granted." },
  { key: "knowledge-bases", label: "Knowledge bases", description: "Curated knowledge agents can search." },
  { key: "project-workspaces", label: "Project workspaces", description: "Workspace resources scoped to projects." },
];

export function findResourceCategory(key: string | undefined): ResourceCategory | undefined {
  if (!key) return undefined;
  return RESOURCE_CATEGORIES.find((c) => c.key === key);
}

// resourceCategoryHref is the real route for a category page.
export function resourceCategoryHref(key: string): string {
  return `/settings/resources/${encodeURIComponent(key)}`;
}
