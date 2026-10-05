// S-204: unified Tools page merge/filter logic.
import { describe, expect, it } from "vitest";
import type { RegistryResource } from "./api";
import type { BuiltinTool } from "./builtinTools";
import {
  BUILTIN_TOOL_DESCRIPTIONS,
  builtinToolItems,
  filterToolItems,
  mergeToolItems,
  registryToolItem,
  toolTileHref,
  type ToolItem,
} from "./toolsPage";

const builtin = (name: string, enabled = true): BuiltinTool => ({
  name: name as BuiltinTool["name"],
  enabled,
  policy: {},
});

const registryTool = (over: Partial<RegistryResource> = {}): RegistryResource => ({
  id: "rt-1",
  type: "tools" as RegistryResource["type"],
  name: "Jira",
  description: "Atlassian Jira integration",
  status: "active",
  ...over,
});

describe("builtinToolItems", () => {
  it("normalises known built-ins with descriptions and route id = name", () => {
    const items = builtinToolItems([builtin("exec"), builtin("web_search", false)]);
    expect(items).toEqual([
      {
        id: "exec",
        kind: "builtin",
        name: "exec",
        description: BUILTIN_TOOL_DESCRIPTIONS.exec,
        enabled: true,
      },
      {
        id: "web_search",
        kind: "builtin",
        name: "web_search",
        description: BUILTIN_TOOL_DESCRIPTIONS.web_search,
        enabled: false,
      },
    ]);
  });

  it("drops unknown tool names defensively", () => {
    expect(builtinToolItems([builtin("not_a_tool")])).toEqual([]);
  });

  it("S-232 regression: keeps every built-in the backend seeds — none silently dropped", () => {
    const all = [
      "exec",
      "web_fetch",
      "web_search",
      "send_message",
      "send_inbox",
      "notify_owner",
      "memory_search",
      "spawn_subagent",
    ].map((n) => builtin(n));
    const items = builtinToolItems(all);
    expect(items.map((i) => i.name)).toEqual(all.map((t) => t.name));
    for (const item of items) {
      expect(item.kind).toBe("builtin");
      expect(item.description.length).toBeGreaterThan(0);
    }
  });

  it("every built-in name has a non-empty description", () => {
    for (const name of ["exec", "web_fetch", "web_search", "send_message", "send_inbox", "notify_owner", "memory_search", "spawn_subagent"] as const) {
      expect(BUILTIN_TOOL_DESCRIPTIONS[name].length).toBeGreaterThan(0);
    }
  });
});

describe("registryToolItem", () => {
  it("maps a registry row onto a tile item", () => {
    expect(registryToolItem(registryTool())).toEqual({
      id: "rt-1",
      kind: "registry",
      name: "Jira",
      description: "Atlassian Jira integration",
      enabled: true,
    });
  });

  it("falls back to the endpoint when there is no description", () => {
    const item = registryToolItem(
      registryTool({ description: undefined, endpoint: "https://jira.example.com" }),
    );
    expect(item.description).toBe("https://jira.example.com");
  });

  it("non-active registry tools are not enabled", () => {
    expect(registryToolItem(registryTool({ status: "deprecated" })).enabled).toBe(false);
  });
});

describe("mergeToolItems", () => {
  it("lists built-ins before registry tools", () => {
    const builtins: ToolItem[] = [
      { id: "exec", kind: "builtin", name: "exec", description: "x", enabled: true },
    ];
    const registry: ToolItem[] = [
      { id: "rt-1", kind: "registry", name: "Jira", description: "y", enabled: true },
    ];
    const merged = mergeToolItems(builtins, registry);
    expect(merged.map((t) => t.kind)).toEqual(["builtin", "registry"]);
  });
});

describe("filterToolItems", () => {
  const items: ToolItem[] = [
    { id: "exec", kind: "builtin", name: "exec", description: "Run terminal commands", enabled: true },
    { id: "rt-1", kind: "registry", name: "Jira", description: "Atlassian integration", enabled: true },
    { id: "rt-2", kind: "registry", name: "GitHub", description: "Repos and PRs", enabled: false },
  ];

  it("blank query is no filter", () => {
    expect(filterToolItems(items, "")).toHaveLength(3);
    expect(filterToolItems(items, "   ")).toHaveLength(3);
  });

  it("matches on name case-insensitively", () => {
    expect(filterToolItems(items, "JIRA").map((t) => t.id)).toEqual(["rt-1"]);
  });

  it("matches on description case-insensitively", () => {
    expect(filterToolItems(items, "terminal").map((t) => t.id)).toEqual(["exec"]);
    expect(filterToolItems(items, "repos").map((t) => t.id)).toEqual(["rt-2"]);
  });

  it("matches across built-ins and registry tools together", () => {
    const hits = filterToolItems(items, "e");
    expect(hits.length).toBeGreaterThan(2);
    expect(new Set(hits.map((t) => t.kind))).toEqual(new Set(["builtin", "registry"]));
  });

  it("no match returns empty", () => {
    expect(filterToolItems(items, "zzz-nothing")).toEqual([]);
  });
});

describe("toolTileHref", () => {
  it("routes built-ins by name", () => {
    expect(toolTileHref("exec")).toBe("/settings/resources/tools/exec");
  });

  it("URL-encodes ids", () => {
    expect(toolTileHref("a b/c")).toBe("/settings/resources/tools/a%20b%2Fc");
  });
});

// S-189 branch coverage: empty-string fallback when a registry row has
// neither description nor endpoint.
describe("registryToolItem empty fallback", () => {
  it("falls back to empty string when description and endpoint are absent", () => {
    const item = registryToolItem(registryTool({ description: undefined, endpoint: undefined }));
    expect(item.description).toBe("");
  });
});
