"use client";

// S-204: unified Tools panel (Settings > Resources > Tools).
//
// Built-in tools are not special-cased into their own screen anymore —
// they're tools the platform ships pre-installed, so this panel merges
// GET /admin/tools (built-ins, platform-admin only per ADR-0012 §2)
// with GET /registry/tools and renders everything as one compact tile
// grid with a name/description search filter. Built-in tiles carry a
// "Built-in" badge and have no delete affordance; registered tools can
// be installed/deleted (registration stays in the section header).
// Clicking any tile opens the tool's configuration page.

import { useMemo, useState } from "react";
import type { RegistryResource } from "../lib/api";
import { apiPatch } from "../lib/api";
import { useAuth } from "../lib/auth";
import { toolsFromList, type BuiltinTool, type BuiltinToolsList } from "../lib/builtinTools";
import {
  builtinToolItems,
  filterToolItems,
  mergeToolItems,
  registryToolItem,
  type ToolItem,
} from "../lib/toolsPage";
import { useApi } from "../lib/useApi";
import { EmptyState } from "./EmptyState";
import { ToolTilesGrid } from "./ToolTiles";

export function ToolsPanel({ isAdmin }: { readonly isAdmin: boolean }) {
  const registry = useApi<RegistryResource[]>("/registry/tools", 60000);
  // Built-in tool config is only readable by platform admins; for other
  // roles the empty path makes useApi a no-op (same visible surface as
  // before S-204: the registry catalog).
  const builtins = useApi<BuiltinToolsList>(isAdmin ? "/admin/tools" : "", 0);
  const { token, mode } = useAuth();
  const [query, setQuery] = useState("");
  // S-241: tile-toggle state. `overrides` carries the optimistic
  // enabled flips until the refreshed GET lands; `pendingToolId` keeps
  // the switch disabled while the PATCH is in flight; `toggleError`
  // surfaces failures in the standard notice style.
  const [overrides, setOverrides] = useState<Record<string, boolean>>({});
  const [pendingToolId, setPendingToolId] = useState("");
  const [toggleError, setToggleError] = useState("");

  const items = useMemo(
    () =>
      filterToolItems(
        mergeToolItems(
          builtinToolItems(toolsFromList(builtins.data)),
          (registry.data ?? []).map(registryToolItem),
        ),
        query,
      ).map((t) => (t.id in overrides ? { ...t, enabled: overrides[t.id] } : t)),
    [builtins.data, registry.data, query, overrides],
  );

  // S-241: enable/disable straight from the tile. Built-ins only — the
  // registry API has no enable endpoint (deprecate is one-way), so
  // ToolTiles renders those switches disabled and this guard is the
  // belt-and-braces twin of that UI restriction.
  function toggleTool(tool: ToolItem, next: boolean) {
    if (tool.kind !== "builtin" || pendingToolId !== "") return;
    setPendingToolId(tool.id);
    setToggleError("");
    setOverrides((o) => ({ ...o, [tool.id]: next }));
    const authedToken = mode === "oidc" ? "" : token;
    apiPatch<BuiltinTool>(`/admin/tools/${tool.name}`, authedToken, { enabled: next })
      .then(() => {
        builtins.refresh();
      })
      .catch((err: unknown) => {
        // Revert the optimistic flip and surface the error like the
        // other panels do.
        setOverrides((o) => {
          const nextOverrides = { ...o };
          delete nextOverrides[tool.id];
          return nextOverrides;
        });
        setToggleError(
          `Could not ${next ? "enable" : "disable"} ${tool.name}: ${err instanceof Error ? err.message : "request failed"}`,
        );
      })
      .finally(() => setPendingToolId(""));
  }

  function renderToolList() {
    if (loading || items.length > 0) {
      return <ToolTilesGrid items={items} onToggle={toggleTool} pendingToolId={pendingToolId} />;
    }
    if (query.trim() !== "") {
      return (
        <EmptyState
          title={`No tools match “${query.trim()}”`}
          hint="Clear the filter to see all tools."
        />
      );
    }
    return (
      <EmptyState
        title="No tools registered"
        hint="Register one so agents can be granted access."
      />
    );
  }

  const loading = registry.loading || (isAdmin && builtins.loading);

  return (
    <>
      <div className="tool-search">
        <input
          type="search"
          placeholder="Filter tools by name or description…"
          value={query}
          onChange={(e) => {
            setQuery(e.target.value);
          }}
          aria-label="Search tools"
        />
      </div>
      {registry.error ? <div className="notice error">{registry.error}</div> : null}
      {isAdmin && builtins.error ? (
        <div className="notice error">{builtins.error}</div>
      ) : null}
      {toggleError ? (
        <div className="notice error" role="alert">
          {toggleError}
        </div>
      ) : null}
      {renderToolList()}
    </>
  );
}
