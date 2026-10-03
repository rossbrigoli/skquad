"use client";

// S-204: compact tool tile grid. Replaces the old vertical "bubble"
// list: each tool is a square-ish tile with a logo (Skquad logo for
// built-ins, a placeholder glyph for registry tools), the tool name,
// and the short description. Clicking a tile opens the tool's
// configuration page. Built-in tiles carry a "Built-in" badge and are
// never deletable (enforced on the config page and the API).

import Image from "next/image";
import Link from "next/link";
import { toolTileHref, type ToolItem } from "../lib/toolsPage";

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

export function ToolTile({ tool }: { readonly tool: ToolItem }) {
  return (
    <Link href={toolTileHref(tool.id)} className="tool-tile" aria-label={`Configure ${tool.name}`}>
      <span className="tool-tile-logo">
        {tool.kind === "builtin" ? (
          // Built-in tools ship with the platform, so they wear the Skquad logo.
          <Image src="/skquad-logo-64.png" width={44} height={44} alt="" />
        ) : (
          <ToolPlaceholderLogo />
        )}
      </span>
      <span className="tool-tile-name">
        {tool.name}
        {tool.kind === "builtin" ? <span className="tool-badge">Built-in</span> : null}
      </span>
      <span className="tool-tile-desc">{tool.description || "—"}</span>
      {!tool.enabled ? <span className="tool-tile-state">disabled</span> : null}
    </Link>
  );
}

export function ToolTilesGrid({ items }: { readonly items: ToolItem[] }) {
  return (
    <div className="tool-grid">
      {items.map((tool) => (
        <ToolTile key={`${tool.kind}:${tool.id}`} tool={tool} />
      ))}
    </div>
  );
}
