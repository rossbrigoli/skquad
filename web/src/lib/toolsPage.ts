// S-204: unified Tools page logic.
//
// Built-in tools are just tools the platform ships pre-installed — they
// live on the same Tools page (Settings > Resources > Tools) as
// registry-registered tools, never on a separate screen. This module is
// the pure, browser-independent merge/filter layer behind the tile grid:
// both the admin API's built-in tool configs (ADR-0012 §2) and the
// resource-registry's tool rows are normalised into one ToolItem shape.

import type { RegistryResource } from "./api";
import {
  BUILTIN_TOOL_NAMES,
  type BuiltinTool,
  type BuiltinToolName,
} from "./builtinTools";

// ToolItem is the unified tile/list representation of a tool.
// `id` is the route id: the built-in tool name ("exec", …) for
// built-ins, the registry UUID for registered tools.
export type ToolItem = {
  id: string;
  kind: "builtin" | "registry";
  name: string;
  description: string;
  enabled: boolean;
};

// BUILTIN_TOOL_DESCRIPTIONS are the short, human descriptions shown on
// tiles. The admin API's ToolConfig carries no description field, so
// these live here (mirroring the hints in the config form).
export const BUILTIN_TOOL_DESCRIPTIONS: Record<BuiltinToolName, string> = {
  exec: "Run terminal commands inside the agent pod sandbox.",
  web_fetch: "Fetch a URL with a guarded GET-only HTTP client.",
  web_search: "Web search through control-plane-held provider keys.",
  send_message: "Agent-to-agent messaging queued by the control plane.",
};

// builtinToolItems normalises loaded built-in configs into tile items,
// defensively dropping anything that isn't a known built-in name.
export function builtinToolItems(tools: BuiltinTool[]): ToolItem[] {
  return tools
    .filter((t) => (BUILTIN_TOOL_NAMES as readonly string[]).includes(t.name))
    .map((t) => ({
      id: t.name,
      kind: "builtin" as const,
      name: t.name,
      description: BUILTIN_TOOL_DESCRIPTIONS[t.name] ?? "",
      enabled: !!t.enabled,
    }));
}

// registryToolItem normalises a registry tool row. Falls back to the
// endpoint for the tile description, matching the old list rows.
export function registryToolItem(resource: RegistryResource): ToolItem {
  return {
    id: resource.id,
    kind: "registry",
    name: resource.name,
    description: resource.description ?? resource.endpoint ?? "",
    enabled: resource.status === "active",
  };
}

// mergeToolItems puts built-ins first — they're the stable platform
// surface, registered tools follow alphabetically as loaded.
export function mergeToolItems(builtins: ToolItem[], registry: ToolItem[]): ToolItem[] {
  return [...builtins, ...registry];
}

// filterToolItems is the tile-grid search: case-insensitive substring
// match on name OR description. A blank/whitespace query is no filter.
export function filterToolItems(items: ToolItem[], query: string): ToolItem[] {
  const q = query.trim().toLowerCase();
  if (q === "") return items;
  return items.filter(
    (t) => t.name.toLowerCase().includes(q) || t.description.toLowerCase().includes(q),
  );
}

// toolTileHref is the config-page route for any tool, built-in or not.
export function toolTileHref(id: string): string {
  return `/settings/resources/tools/${encodeURIComponent(id)}`;
}
