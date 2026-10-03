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
import { toolsFromList, type BuiltinToolsList } from "../lib/builtinTools";
import {
  builtinToolItems,
  filterToolItems,
  mergeToolItems,
  registryToolItem,
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
  const [query, setQuery] = useState("");

  const items = useMemo(
    () =>
      filterToolItems(
        mergeToolItems(
          builtinToolItems(toolsFromList(builtins.data)),
          (registry.data ?? []).map(registryToolItem),
        ),
        query,
      ),
    [builtins.data, registry.data, query],
  );

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
      {!loading && items.length === 0 ? (
        query.trim() !== "" ? (
          <EmptyState
            title={`No tools match “${query.trim()}”`}
            hint="Clear the filter to see all tools."
          />
        ) : (
          <EmptyState
            title="No tools registered"
            hint="Register one so agents can be granted access."
          />
        )
      ) : (
        <ToolTilesGrid items={items} />
      )}
    </>
  );
}
