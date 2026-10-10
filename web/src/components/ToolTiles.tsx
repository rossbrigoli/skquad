"use client";

// S-204: compact tool tile grid. Replaces the old vertical "bubble"
// list: each tool is a square-ish tile with a logo (a wrench glyph for
// built-ins since S-241, a placeholder plug glyph for registry tools),
// the tool name, and the short description. Clicking a tile opens the
// tool's configuration page. Built-in tiles carry a "Built-in" badge
// and are never deletable (enforced on the config page and the API).
//
// S-241: every tile also carries an enable/disable ToggleSwitch so the
// user can flip a tool without opening its config page. The toggle is a
// SIBLING of the <Link> (nesting interactive elements inside an <a> is
// invalid HTML), absolutely positioned in the tile's top-right corner.
// Only built-in tools can be toggled from the tile (PATCH
// /admin/tools/{name} merge-patches `enabled`); registry tools render
// the toggle disabled because the registry API has no enable endpoint
// (deprecation is one-way — see control-plane /deprecate).

import Link from "next/link";
import { toolTileHref, type ToolItem } from "../lib/toolsPage";
import { ToggleSwitch } from "./ToggleSwitch";

// ToolPlaceholderLogo is the stand-in artwork for registered tools that
// have no logo of their own (a generic plug glyph).
export function ToolPlaceholderLogo() {
  return (
    <svg width="28" height="28" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path
        d="M9 3v4M15 3v4M7 7h10v4a5 5 0 0 1-10 0V7ZM12 16v5"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

// ToolIcon (S-241) replaces the Skquad logo on built-in tool tiles —
// built-ins are tools, not branding. Hand-rolled wrench glyph in the
// same stroke style as ToolPlaceholderLogo.
export function ToolIcon() {
  return (
    <svg width="28" height="28" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path
        d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}

// ToolTile renders one tile: the configure link plus the enable/disable
// toggle. `onToggle` is provided by ToolsPanel for built-in tools only;
// registry tiles render the switch disabled with an explanatory label.
export function ToolTile({
  tool,
  onToggle,
  pending = false,
}: {
  readonly tool: ToolItem;
  readonly onToggle?: (tool: ToolItem, next: boolean) => void;
  readonly pending?: boolean;
}) {
  const toggleable = tool.kind === "builtin" && onToggle !== undefined;
  const toggleVerb = tool.enabled ? "Disable" : "Enable";
  const toggleLabel = toggleable
    ? `${toggleVerb} ${tool.name}`
    : `${tool.name}: enable/disable is managed on the registry, not from the tile`;
  return (
    <div className="tool-tile-wrap">
      <Link href={toolTileHref(tool.id)} className="tool-tile" aria-label={`Configure ${tool.name}`}>
        <span className="tool-tile-logo">
          {tool.kind === "builtin" ? <ToolIcon /> : <ToolPlaceholderLogo />}
        </span>
        <span className="tool-tile-name">
          {tool.name}
          {tool.kind === "builtin" ? <span className="tool-badge">Built-in</span> : null}
        </span>
        <span className="tool-tile-desc">{tool.description || "—"}</span>
        {!tool.enabled ? <span className="tool-tile-state">disabled</span> : null}
      </Link>
      <span className="tool-tile-toggle">
        <ToggleSwitch
          checked={tool.enabled}
          disabled={!toggleable || pending}
          label={toggleLabel}
          onToggle={(next) => onToggle?.(tool, next)}
        />
      </span>
    </div>
  );
}

export function ToolTilesGrid({
  items,
  onToggle,
  pendingToolId = "",
}: {
  readonly items: ToolItem[];
  readonly onToggle?: (tool: ToolItem, next: boolean) => void;
  readonly pendingToolId?: string;
}) {
  return (
    <div className="tool-grid">
      {items.map((tool) => (
        <ToolTile
          key={`${tool.kind}:${tool.id}`}
          tool={tool}
          onToggle={onToggle}
          pending={pendingToolId === tool.id}
        />
      ))}
    </div>
  );
}
